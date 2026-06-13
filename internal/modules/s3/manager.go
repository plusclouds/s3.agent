package s3

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// doS3 creates an HTTP request against the S3 gateway, signs it with AWS Sig V4
// if admin credentials are configured, and executes it.
func (m *Manager) doS3(ctx context.Context, method, rawURL string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	signAWSv4(req, m.cfg.AdminAccessKey, m.cfg.AdminSecretKey, m.cfg.S3Region, body)
	return m.client.Do(req)
}

// ListBuckets returns the names of all buckets visible on the S3 gateway.
func (m *Manager) ListBuckets(ctx context.Context) ([]string, error) {
	resp, err := m.doS3(ctx, http.MethodGet, m.cfg.S3URL+"/", nil)
	if err != nil {
		return nil, fmt.Errorf("listing buckets: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("listing buckets: HTTP %d", resp.StatusCode)
	}

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

// CreateBucket creates a bucket on the S3 gateway via a signed PUT request.
func (m *Manager) CreateBucket(ctx context.Context, spec BucketSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("bucket name is required")
	}
	resp, err := m.doS3(ctx, http.MethodPut, m.cfg.S3URL+"/"+spec.Name, nil)
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

// UpdateBucket applies updates to an existing bucket. Currently only lifecycle
// rules are applied; if none are provided the call is a validated no-op.
func (m *Manager) UpdateBucket(ctx context.Context, spec BucketSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("bucket name is required")
	}
	if len(spec.LifecycleRules) == 0 {
		return nil
	}
	return m.putLifecycle(ctx, spec.Name, spec.LifecycleRules)
}

// CreateWORMBucket creates a bucket with S3 Object Lock (WORM) enabled and sets
// the default retention policy. Object Lock cannot be disabled after creation.
func (m *Manager) CreateWORMBucket(ctx context.Context, spec WORMBucketSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("bucket name is required")
	}
	if spec.Mode != "COMPLIANCE" && spec.Mode != "GOVERNANCE" {
		return fmt.Errorf("object_lock_mode must be COMPLIANCE or GOVERNANCE, got %q", spec.Mode)
	}
	if spec.RetentionDays <= 0 {
		return fmt.Errorf("retention_days must be > 0")
	}

	rawURL := m.cfg.S3URL + "/" + spec.Name
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, rawURL, http.NoBody)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("x-amz-bucket-object-lock-enabled", "true")
	signAWSv4(req, m.cfg.AdminAccessKey, m.cfg.AdminSecretKey, m.cfg.S3Region, nil)

	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("creating WORM bucket %s: %w", spec.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("creating WORM bucket %s: HTTP %d", spec.Name, resp.StatusCode)
	}

	if err := m.putObjectLock(ctx, spec.Name, spec.Mode, spec.RetentionDays); err != nil {
		return err
	}

	m.logger.Info("WORM bucket created",
		zap.String("bucket", spec.Name),
		zap.String("mode", spec.Mode),
		zap.Int("retention_days", spec.RetentionDays),
	)
	return nil
}

// UpdateWORMBucket updates the default retention policy on an existing WORM bucket.
// Object Lock mode and retention period can be changed; Object Lock cannot be disabled.
func (m *Manager) UpdateWORMBucket(ctx context.Context, spec WORMBucketSpec) error {
	if spec.Name == "" {
		return fmt.Errorf("bucket name is required")
	}
	if spec.Mode != "COMPLIANCE" && spec.Mode != "GOVERNANCE" {
		return fmt.Errorf("object_lock_mode must be COMPLIANCE or GOVERNANCE, got %q", spec.Mode)
	}
	if spec.RetentionDays <= 0 {
		return fmt.Errorf("retention_days must be > 0")
	}

	if err := m.putObjectLock(ctx, spec.Name, spec.Mode, spec.RetentionDays); err != nil {
		return err
	}

	m.logger.Info("WORM bucket updated",
		zap.String("bucket", spec.Name),
		zap.String("mode", spec.Mode),
		zap.Int("retention_days", spec.RetentionDays),
	)
	return nil
}

// DeleteWORMBucket deletes a WORM bucket. The bucket must be empty — objects
// within their retention period cannot be deleted (COMPLIANCE) or require
// bypass permission (GOVERNANCE). No force-empty is attempted.
func (m *Manager) DeleteWORMBucket(ctx context.Context, name string) error {
	if name == "" {
		return fmt.Errorf("bucket name is required")
	}
	resp, err := m.doS3(ctx, http.MethodDelete, m.cfg.S3URL+"/"+name, nil)
	if err != nil {
		return fmt.Errorf("deleting WORM bucket %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("deleting WORM bucket %s: HTTP %d: %s", name, resp.StatusCode, body)
	}
	m.logger.Info("WORM bucket deleted", zap.String("bucket", name))
	return nil
}

// putObjectLock sets the default Object Lock retention configuration on a bucket.
func (m *Manager) putObjectLock(ctx context.Context, bucket, mode string, days int) error {
	body := fmt.Sprintf(
		`<?xml version="1.0" encoding="UTF-8"?>`+
			`<ObjectLockConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+
			`<ObjectLockEnabled>Enabled</ObjectLockEnabled>`+
			`<Rule><DefaultRetention><Mode>%s</Mode><Days>%d</Days></DefaultRetention></Rule>`+
			`</ObjectLockConfiguration>`,
		mode, days,
	)
	rawURL := m.cfg.S3URL + "/" + bucket + "?object-lock"
	resp, err := m.doS3(ctx, http.MethodPut, rawURL, []byte(body))
	if err != nil {
		return fmt.Errorf("setting object lock on %s: %w", bucket, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("setting object lock on %s: HTTP %d: %s", bucket, resp.StatusCode, respBody)
	}
	return nil
}

// putLifecycle sets S3 lifecycle rules on a bucket via the AWS lifecycle API.
func (m *Manager) putLifecycle(ctx context.Context, bucket string, rules []LifecycleRule) error {
	var sb strings.Builder
	sb.WriteString(`<LifecycleConfiguration>`)
	for i, r := range rules {
		sb.WriteString(fmt.Sprintf(
			`<Rule><ID>rule-%d</ID><Filter><Prefix>%s</Prefix></Filter>`+
				`<Status>Enabled</Status><Expiration><Days>%d</Days></Expiration></Rule>`,
			i+1, r.Prefix, r.ExpireDays,
		))
	}
	sb.WriteString(`</LifecycleConfiguration>`)

	body := []byte(sb.String())
	rawURL := m.cfg.S3URL + "/" + bucket + "?lifecycle"
	resp, err := m.doS3(ctx, http.MethodPut, rawURL, body)
	if err != nil {
		return fmt.Errorf("setting lifecycle on %s: %w", bucket, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("setting lifecycle on %s: HTTP %d", bucket, resp.StatusCode)
	}

	m.logger.Info("lifecycle rules applied", zap.String("bucket", bucket), zap.Int("rules", len(rules)))
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

	resp, err := m.doS3(ctx, http.MethodDelete, m.cfg.S3URL+"/"+name, nil)
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
	resp, err := m.doS3(ctx, http.MethodGet, m.cfg.S3URL+"/"+name+"?list-type=2", nil)
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
		delResp, err := m.doS3(ctx, http.MethodDelete, m.cfg.S3URL+"/"+name+"/"+obj.Key, nil)
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

// EnsureAdmin ensures the system admin credential (from s3.admin_access_key) exists
// in s3.json. Called at startup to recover if Reconcile wiped the admin account.
// No-op when admin_access_key is not configured.
func (m *Manager) EnsureAdmin() error {
	if m.cfg.AdminAccessKey == "" {
		return nil
	}
	iam, err := m.readIAM()
	if err != nil {
		return fmt.Errorf("reading IAM for admin check: %w", err)
	}
	for _, id := range iam.Identities {
		for _, cred := range id.Credentials {
			if cred.AccessKey == m.cfg.AdminAccessKey {
				return nil // already present
			}
		}
	}
	iam.Identities = append(iam.Identities, iamIdentity{
		Name:        "plusclouds-s3-admin",
		Credentials: []iamCredential{{AccessKey: m.cfg.AdminAccessKey, SecretKey: m.cfg.AdminSecretKey}},
		Actions:     []string{"Admin"},
	})
	if err := m.writeIAM(iam); err != nil {
		return err
	}
	m.logger.Info("seeded system admin credential in s3.json")
	return m.reloadWeedS3()
}

// isSystemAdmin reports whether the given access key is the reserved system admin.
func (m *Manager) isSystemAdmin(accessKey string) bool {
	return m.cfg.AdminAccessKey != "" && accessKey == m.cfg.AdminAccessKey
}

// ListIAM returns all IAM identities from s3.json with secret keys stripped.
// The system admin account (admin_access_key) is excluded — it is not managed
// by the platform.
func (m *Manager) ListIAM() ([]IAMUser, error) {
	iam, err := m.readIAM()
	if err != nil {
		return nil, err
	}
	users := make([]IAMUser, 0, len(iam.Identities))
	for _, id := range iam.Identities {
		accessKey := ""
		if len(id.Credentials) > 0 {
			accessKey = id.Credentials[0].AccessKey
		}
		if m.isSystemAdmin(accessKey) {
			continue // system admin is not platform-managed
		}
		users = append(users, IAMUser{
			Name:      id.Name,
			AccessKey: accessKey,
			// SecretKey intentionally omitted
		})
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// weed-s3 has no ExecReload= in its unit; use restart to pick up s3.json changes.
	_, _, err := m.exec.Execute(ctx, "systemctl", "restart", svc)
	if err != nil {
		return fmt.Errorf("restarting %s: %w", svc, err)
	}
	m.logger.Info("restarted SeaweedFS S3 service", zap.String("service", svc))
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
// If path already exists, the temp file inherits its uid/gid so ownership is
// preserved after the rename (important when the agent runs as root but the
// target file is owned by a service user, e.g. seaweedfs).
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

	// Preserve ownership of the existing file so a service user (e.g. seaweedfs)
	// can still read the file after it is replaced.
	if info, err := os.Stat(path); err == nil {
		if stat, ok := info.Sys().(*syscall.Stat_t); ok {
			_ = os.Lchown(tmpName, int(stat.Uid), int(stat.Gid))
		}
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
