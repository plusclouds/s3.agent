package s3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/config"
	"github.com/plusclouds/ubuntu-agent/internal/executor"
)

// Manager executes S3 management operations: bucket CRUD via the SeaweedFS S3
// API, IAM identity management via s3.json, and Nginx customer blocking via
// s3_blocked_keys.conf.
type Manager struct {
	cfg    config.S3Config
	exec   *executor.Executor
	client *http.Client
	logger *zap.Logger
}

// NewManager creates a Manager.
func NewManager(cfg config.S3Config, exec *executor.Executor, logger *zap.Logger) *Manager {
	return &Manager{
		cfg:    cfg,
		exec:   exec,
		client: &http.Client{Timeout: 15 * time.Second},
		logger: logger,
	}
}

// ---------------------------------------------------------------------------
// Bucket operations (S3 API → :8333)
// ---------------------------------------------------------------------------

// ListBuckets returns the names of all buckets visible on the S3 gateway.
func (m *Manager) ListBuckets(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cfg.S3URL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("listing buckets: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		Buckets struct {
			Bucket []struct {
				Name string `xml:"Name"`
			} `xml:"Bucket"`
		} `xml:"Buckets"`
	}
	if err := decodeXML(resp.Body, &result); err != nil {
		return nil, fmt.Errorf("parsing bucket list: %w", err)
	}

	names := make([]string, 0, len(result.Buckets.Bucket))
	for _, b := range result.Buckets.Bucket {
		names = append(names, b.Name)
	}
	return names, nil
}

// CreateBucket creates a bucket on the S3 gateway via a PUT request.
func (m *Manager) CreateBucket(ctx context.Context, spec BucketSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("bucket name is required")
	}
	url := m.cfg.S3URL + "/" + spec.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating bucket %s: %w", spec.Name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("creating bucket %s: HTTP %d", spec.Name, resp.StatusCode)
	}

	m.logger.Info("bucket created", zap.String("bucket", spec.Name))
	return nil
}

// DeleteBucket deletes a bucket from the S3 gateway. If forceEmpty is true,
// all objects are deleted first.
func (m *Manager) DeleteBucket(ctx context.Context, name string, forceEmpty bool) error {
	if name == "" {
		return fmt.Errorf("bucket name is required")
	}

	if forceEmpty {
		if err := m.emptyBucket(ctx, name); err != nil {
			return fmt.Errorf("emptying bucket %s: %w", name, err)
		}
	}

	url := m.cfg.S3URL + "/" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("deleting bucket %s: %w", name, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("deleting bucket %s: HTTP %d", name, resp.StatusCode)
	}

	m.logger.Info("bucket deleted", zap.String("bucket", name))
	return nil
}

// emptyBucket lists all objects in a bucket and deletes them.
func (m *Manager) emptyBucket(ctx context.Context, name string) error {
	url := m.cfg.S3URL + "/" + name + "?list-type=2"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var listResult struct {
		Contents []struct {
			Key string `xml:"Key"`
		} `xml:"Contents"`
	}
	if err := decodeXML(resp.Body, &listResult); err != nil {
		return fmt.Errorf("listing objects: %w", err)
	}

	for _, obj := range listResult.Contents {
		delURL := m.cfg.S3URL + "/" + name + "/" + obj.Key
		delReq, err := http.NewRequestWithContext(ctx, http.MethodDelete, delURL, nil)
		if err != nil {
			return err
		}
		delResp, err := m.client.Do(delReq)
		if err != nil {
			return fmt.Errorf("deleting object %s: %w", obj.Key, err)
		}
		delResp.Body.Close()
	}
	return nil
}

// ---------------------------------------------------------------------------
// IAM operations (s3.json file)
// ---------------------------------------------------------------------------

// iamFile is the raw JSON structure of /etc/seaweedfs/s3.json.
type iamFile struct {
	Identities []iamIdentity `json:"identities"`
}

type iamIdentity struct {
	Name        string          `json:"name"`
	Credentials []iamCredential `json:"credentials"`
	Actions     []string        `json:"actions"`
}

type iamCredential struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// ListIAM returns all IAM identities from s3.json with secret keys stripped.
func (m *Manager) ListIAM() ([]IAMUser, error) {
	iam, err := m.readIAM()
	if err != nil {
		return nil, err
	}
	users := make([]IAMUser, 0, len(iam.Identities))
	for _, id := range iam.Identities {
		user := IAMUser{
			Name: id.Name,
		}
		if len(id.Credentials) > 0 {
			user.AccessKey = id.Credentials[0].AccessKey
			// SecretKey intentionally omitted
		}
		users = append(users, user)
	}
	return users, nil
}

// CreateIAMUser appends a new identity to s3.json and reloads weed-s3.
func (m *Manager) CreateIAMUser(user IAMUser, secretKey string) error {
	iam, err := m.readIAM()
	if err != nil {
		return err
	}

	// Build actions from BucketACLs if no explicit action strings provided.
	actions := m.buildActions(user)

	iam.Identities = append(iam.Identities, iamIdentity{
		Name: user.Name,
		Credentials: []iamCredential{
			{AccessKey: user.AccessKey, SecretKey: secretKey},
		},
		Actions: actions,
	})

	if err := m.writeIAM(iam); err != nil {
		return err
	}

	m.logger.Info("IAM user created", zap.String("name", user.Name))
	return m.reloadWeedS3()
}

// DeleteIAMUser removes an identity from s3.json and reloads weed-s3.
func (m *Manager) DeleteIAMUser(name string) error {
	iam, err := m.readIAM()
	if err != nil {
		return err
	}

	filtered := iam.Identities[:0]
	found := false
	for _, id := range iam.Identities {
		if id.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, id)
	}
	if !found {
		return fmt.Errorf("IAM user %q not found", name)
	}
	iam.Identities = filtered

	if err := m.writeIAM(iam); err != nil {
		return err
	}

	m.logger.Info("IAM user deleted", zap.String("name", name))
	return m.reloadWeedS3()
}

func (m *Manager) buildActions(user IAMUser) []string {
	if len(user.BucketACLs) == 0 {
		return []string{"Read", "Write", "List", "Tagging", "Admin"}
	}
	var actions []string
	for _, acl := range user.BucketACLs {
		prefix := acl.BucketID + "-*"
		switch acl.Permission {
		case "r":
			actions = append(actions, "Read:"+prefix, "List:"+prefix)
		case "rw":
			actions = append(actions, "Read:"+prefix, "Write:"+prefix, "List:"+prefix, "Tagging:"+prefix)
		default: // "admin" or unknown
			actions = append(actions, "Read:"+prefix, "Write:"+prefix, "List:"+prefix, "Tagging:"+prefix, "Admin:"+prefix)
		}
	}
	return actions
}

func (m *Manager) readIAM() (*iamFile, error) {
	data, err := os.ReadFile(m.cfg.IAMFile)
	if err != nil {
		if os.IsNotExist(err) {
			return &iamFile{}, nil
		}
		return nil, fmt.Errorf("reading %s: %w", m.cfg.IAMFile, err)
	}
	var iam iamFile
	if err := json.Unmarshal(data, &iam); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", m.cfg.IAMFile, err)
	}
	return &iam, nil
}

func (m *Manager) writeIAM(iam *iamFile) error {
	data, err := json.MarshalIndent(iam, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling IAM: %w", err)
	}
	return atomicWrite(m.cfg.IAMFile, data, 0640)
}

func (m *Manager) reloadWeedS3() error {
	svc := m.cfg.WeedS3Service
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := m.exec.Execute(ctx, "systemctl", "reload", svc)
	if err != nil {
		return fmt.Errorf("reloading %s: %w", svc, err)
	}
	m.logger.Info("reloaded SeaweedFS S3 service", zap.String("service", svc))
	return nil
}

// ---------------------------------------------------------------------------
// Nginx blocking (s3_blocked_keys.conf)
// ---------------------------------------------------------------------------

// ListBlocked returns the access keys currently blocked in s3_blocked_keys.conf.
func (m *Manager) ListBlocked() ([]string, error) {
	data, err := os.ReadFile(m.cfg.NginxBlockedKeysFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", m.cfg.NginxBlockedKeysFile, err)
	}

	var keys []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, `if ($http_authorization ~*`) {
			// Extract the key from: if ($http_authorization ~* "KEYVALUE") {
			start := strings.Index(line, `"`)
			end := strings.LastIndex(line, `"`)
			if start >= 0 && end > start {
				keys = append(keys, line[start+1:end])
			}
		}
	}
	return keys, nil
}

// BlockAccessKey adds a customer access key to s3_blocked_keys.conf and reloads Nginx.
func (m *Manager) BlockAccessKey(accessKey, reason string) error {
	if accessKey == "" {
		return fmt.Errorf("access_key is required")
	}

	data, err := os.ReadFile(m.cfg.NginxBlockedKeysFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading blocked keys file: %w", err)
	}

	// Check if already blocked.
	if strings.Contains(string(data), `"`+accessKey+`"`) {
		return nil
	}

	entry := fmt.Sprintf("\n# %s (blocked %s)\nif ($http_authorization ~* \"%s\") {\n    return 403 \"Storage quota exceeded. Please contact support.\";\n}\n",
		reason, time.Now().Format("2006-01-02"), accessKey)

	newData := append(data, []byte(entry)...)
	if err := atomicWrite(m.cfg.NginxBlockedKeysFile, newData, 0644); err != nil {
		return err
	}

	m.logger.Info("access key blocked", zap.String("access_key", accessKey), zap.String("reason", reason))
	return m.reloadNginx()
}

// UnblockAccessKey removes a customer access key from s3_blocked_keys.conf and reloads Nginx.
func (m *Manager) UnblockAccessKey(accessKey string) error {
	if accessKey == "" {
		return fmt.Errorf("access_key is required")
	}

	data, err := os.ReadFile(m.cfg.NginxBlockedKeysFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("reading blocked keys file: %w", err)
	}

	// Remove the comment line, the if block, and the closing brace for this key.
	lines := strings.Split(string(data), "\n")
	var out []string
	skip := 0
	for i, line := range lines {
		if skip > 0 {
			skip--
			continue
		}
		// Detect the comment line before the if block (# reason (blocked DATE))
		// followed by the if block referencing this key.
		if strings.HasPrefix(strings.TrimSpace(line), "#") &&
			i+1 < len(lines) &&
			strings.Contains(lines[i+1], `"`+accessKey+`"`) {
			skip = 3 // skip comment, if-line, return-line, closing brace
			continue
		}
		if strings.Contains(line, `"`+accessKey+`"`) {
			skip = 2 // skip if-line, return-line, closing brace
			continue
		}
		out = append(out, line)
	}

	newData := []byte(strings.Join(out, "\n"))
	if err := atomicWrite(m.cfg.NginxBlockedKeysFile, newData, 0644); err != nil {
		return err
	}

	m.logger.Info("access key unblocked", zap.String("access_key", accessKey))
	return m.reloadNginx()
}

func (m *Manager) reloadNginx() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _, err := m.exec.Execute(ctx, "nginx", "-s", "reload")
	if err != nil {
		return fmt.Errorf("reloading nginx: %w", err)
	}
	m.logger.Info("nginx reloaded")
	return nil
}

// ---------------------------------------------------------------------------
// Reconcile
// ---------------------------------------------------------------------------

// Reconcile computes the diff between the desired FullSyncPayload and the
// current actual state (live buckets + IAM file), then applies creates and
// deletes. Errors from individual items are collected and returned in the
// result rather than aborting the entire reconcile.
func (m *Manager) Reconcile(ctx context.Context, desired FullSyncPayload) (*ReconcileResult, error) {
	result := &ReconcileResult{}

	// --- Buckets ---
	actualBuckets, err := m.ListBuckets(ctx)
	if err != nil {
		return result, fmt.Errorf("listing actual buckets: %w", err)
	}
	actualBucketSet := make(map[string]bool, len(actualBuckets))
	for _, b := range actualBuckets {
		actualBucketSet[b] = true
	}

	desiredBucketSet := make(map[string]bool, len(desired.Buckets))
	for _, spec := range desired.Buckets {
		desiredBucketSet[spec.Name] = true
	}

	// Create missing buckets.
	for _, spec := range desired.Buckets {
		if !actualBucketSet[spec.Name] {
			if err := m.CreateBucket(ctx, spec); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("create bucket %s: %v", spec.Name, err))
			} else {
				result.BucketsCreated++
			}
		}
	}

	// Delete extra buckets (present in actual but not in desired).
	for _, name := range actualBuckets {
		if !desiredBucketSet[name] {
			if err := m.DeleteBucket(ctx, name, false); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("delete bucket %s: %v", name, err))
			} else {
				result.BucketsDeleted++
			}
		}
	}

	// --- IAM ---
	actualIAM, err := m.ListIAM()
	if err != nil {
		return result, fmt.Errorf("listing actual IAM: %w", err)
	}
	actualIAMSet := make(map[string]bool, len(actualIAM))
	for _, u := range actualIAM {
		actualIAMSet[u.Name] = true
	}

	desiredIAMSet := make(map[string]bool, len(desired.IAMUsers))
	for _, u := range desired.IAMUsers {
		desiredIAMSet[u.Name] = true
	}

	// Create missing IAM users.
	for _, u := range desired.IAMUsers {
		if !actualIAMSet[u.Name] {
			if err := m.CreateIAMUser(u, u.SecretKey); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("create IAM user %s: %v", u.Name, err))
			} else {
				result.IAMCreated++
			}
		}
	}

	// Delete extra IAM users.
	for _, u := range actualIAM {
		if !desiredIAMSet[u.Name] {
			if err := m.DeleteIAMUser(u.Name); err != nil {
				result.Errors = append(result.Errors, fmt.Sprintf("delete IAM user %s: %v", u.Name, err))
			} else {
				result.IAMDeleted++
			}
		}
	}

	return result, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// atomicWrite writes data to path by writing to a temp file and renaming.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".s3d-write-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName) // no-op after successful rename
	}()

	if err := tmp.Chmod(perm); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("renaming temp file to %s: %w", path, err)
	}
	return nil
}

// decodeXML decodes XML from an io.Reader — declared in a single place to
// avoid import duplication between files.
func decodeXML(r interface{ Read([]byte) (int, error) }, v any) error {
	return xml.NewDecoder(r).Decode(v)
}
