package s3

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/config"
	"github.com/plusclouds/ubuntu-agent/internal/modules/services"
)

// Observer polls the SeaweedFS cluster and systemd services, reporting
// ClusterComponents to callers. It triggers Watch callbacks immediately when
// component state changes are detected.
type Observer struct {
	cfg     config.S3Config
	svc     services.Manager
	client  *http.Client
	logger  *zap.Logger
	traffic *trafficCollector
	audit   *auditTailer
}

// NewObserver creates an Observer.
func NewObserver(cfg config.S3Config, svc services.Manager, logger *zap.Logger) *Observer {
	client := &http.Client{Timeout: 5 * time.Second}
	return &Observer{
		cfg:     cfg,
		svc:     svc,
		client:  client,
		logger:  logger,
		traffic: newTrafficCollector(cfg.NginxAccessLog),
		audit:   newAuditTailer(cfg, client, logger),
	}
}

// RunAudit starts the background goroutine that tails the Nginx audit log and
// emits AuditEvents. Call once from publisher.Start.
func (o *Observer) RunAudit(ctx context.Context) {
	go o.audit.run(ctx)
}

// AuditEvents returns the channel that receives mutation events from the audit
// tailer. The publisher drains this and publishes TypeS3Audit envelopes.
func (o *Observer) AuditEvents() <-chan AuditEvent {
	return o.audit.ch
}

// FlushTraffic reads new Nginx access log lines since the last call and returns
// per-bucket traffic deltas. Resets the window on each call — callers receive
// only the traffic that occurred since their previous call.
func (o *Observer) FlushTraffic() []TrafficStat {
	return o.traffic.flush(o.logger)
}

// ---------------------------------------------------------------------------
// trafficCollector — Nginx access log tailer
// ---------------------------------------------------------------------------

// trafficCollector tails the Nginx access log and aggregates per-bucket traffic.
// The log format must be set to s3_traffic in nginx (see configs/nginx/).
// Log line format: $msec $request_method $request_uri $status $bytes_sent $request_length
type trafficCollector struct {
	path   string
	mu     sync.Mutex
	offset int64
}

func newTrafficCollector(path string) *trafficCollector {
	return &trafficCollector{path: path}
}

func (tc *trafficCollector) flush(logger *zap.Logger) []TrafficStat {
	tc.mu.Lock()
	defer tc.mu.Unlock()

	f, err := os.Open(tc.path)
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("traffic: cannot open nginx access log", zap.String("path", tc.path), zap.Error(err))
		}
		return nil
	}
	defer f.Close()

	// Detect log rotation: if stored offset exceeds file size, reset to beginning.
	if info, err := f.Stat(); err == nil && tc.offset > info.Size() {
		tc.offset = 0
	}

	if _, err := f.Seek(tc.offset, io.SeekStart); err != nil {
		return nil
	}

	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil
	}
	tc.offset += int64(len(data))

	acc := make(map[string]*TrafficStat)
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		b, in, out := parseTrafficLine(line)
		if b == "" {
			continue
		}
		ts := acc[b]
		if ts == nil {
			ts = &TrafficStat{BucketName: b}
			acc[b] = ts
		}
		ts.BytesIn += in
		ts.BytesOut += out
		ts.Requests++
	}

	result := make([]TrafficStat, 0, len(acc))
	for _, ts := range acc {
		result = append(result, *ts)
	}
	return result
}

// parseTrafficLine parses one line from the s3_traffic nginx log format.
// Returns bucket name, bytes_in (upload), bytes_out (download).
// Returns empty bucket on any parse error or non-2xx status.
//
// Log format: $msec $request_method $request_uri $status $bytes_sent $request_length
func parseTrafficLine(line string) (bucket string, bytesIn, bytesOut int64) {
	f := strings.Fields(line)
	if len(f) < 6 {
		return "", 0, 0
	}
	// f[0]=msec f[1]=method f[2]=uri f[3]=status f[4]=bytes_sent f[5]=request_length
	method := f[1]
	uri := f[2]
	status, _ := strconv.Atoi(f[3])
	bytesSent, _ := strconv.ParseInt(f[4], 10, 64)
	reqLength, _ := strconv.ParseInt(f[5], 10, 64)

	if status < 200 || status >= 300 {
		return "", 0, 0
	}

	bucket = extractBucketFromURI(uri)
	if bucket == "" {
		return "", 0, 0
	}

	switch method {
	case "PUT", "POST":
		bytesIn = reqLength
	case "GET":
		bytesOut = bytesSent
	}
	return bucket, bytesIn, bytesOut
}

// extractBucketFromURI returns the first path segment of an S3 URI.
// e.g. "/my-bucket/obj.txt?versionId=1" → "my-bucket"
func extractBucketFromURI(uri string) string {
	if idx := strings.IndexByte(uri, '?'); idx >= 0 {
		uri = uri[:idx]
	}
	uri = strings.TrimPrefix(uri, "/")
	if slash := strings.IndexByte(uri, '/'); slash >= 0 {
		uri = uri[:slash]
	}
	return uri
}

// ---------------------------------------------------------------------------
// auditTailer — real-time mutation event tailer
// ---------------------------------------------------------------------------

// auditTailer tails the Nginx s3_audit log and emits one AuditEvent per
// data-mutation line (PUT, POST, DELETE with 2xx status) to a buffered channel.
// Log line format (space-delimited, auth header quoted):
//
//	$msec $request_method $request_uri $status $request_length $remote_addr "$http_authorization"
type auditTailer struct {
	cfg    config.S3Config
	client *http.Client
	logger *zap.Logger
	mu     sync.Mutex
	offset int64
	ch     chan AuditEvent
}

func newAuditTailer(cfg config.S3Config, client *http.Client, logger *zap.Logger) *auditTailer {
	return &auditTailer{
		cfg:    cfg,
		client: client,
		logger: logger,
		ch:     make(chan AuditEvent, 512),
	}
}

func (t *auditTailer) run(ctx context.Context) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.poll()
		}
	}
}

func (t *auditTailer) poll() {
	lines := t.readNewLines()
	for _, line := range lines {
		evt, ok := parseAuditLine(line)
		if !ok {
			continue
		}
		// For PUT operations on a specific object key, attempt to read the
		// WORM retain-until date from the S3 Object Lock header.
		if evt.Action == "PUT" && evt.ObjectKey != "" {
			evt.RetainUntil = t.queryRetainUntil(evt.Bucket, evt.ObjectKey)
		}
		select {
		case t.ch <- evt:
		default:
			t.logger.Warn("audit: event channel full, dropping event",
				zap.String("bucket", evt.Bucket), zap.String("key", evt.ObjectKey))
		}
	}
}

// readNewLines reads all log lines written since the last call.
// Holds the mutex only during file I/O so HTTP calls in poll() are lock-free.
func (t *auditTailer) readNewLines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()

	f, err := os.Open(t.cfg.NginxAuditLog)
	if err != nil {
		if !os.IsNotExist(err) {
			t.logger.Warn("audit: cannot open log", zap.String("path", t.cfg.NginxAuditLog), zap.Error(err))
		}
		return nil
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && t.offset > info.Size() {
		t.offset = 0 // log was rotated
	}
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return nil
	}
	data, err := io.ReadAll(f)
	if err != nil || len(data) == 0 {
		return nil
	}
	t.offset += int64(len(data))

	raw := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// queryRetainUntil performs a signed HEAD request and returns the
// x-amz-object-lock-retain-until-date header value, or nil if absent.
func (t *auditTailer) queryRetainUntil(bucket, key string) *time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodHead,
		t.cfg.S3URL+"/"+bucket+"/"+key, nil)
	if err != nil {
		return nil
	}
	signAWSv4(req, t.cfg.AdminAccessKey, t.cfg.AdminSecretKey, t.cfg.S3Region, nil)

	resp, err := t.client.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	resp.Body.Close()

	raw := resp.Header.Get("x-amz-object-lock-retain-until-date")
	if raw == "" {
		return nil
	}
	rv, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return nil
	}
	return &rv
}

// parseAuditLine parses one line from the s3_audit Nginx log format.
// Format: $msec $method $uri $status $request_length $remote_addr "$http_authorization"
// Returns false on any parse error, non-mutation method, or non-2xx status.
func parseAuditLine(line string) (AuditEvent, bool) {
	// Split into exactly 7 parts; the last part is the quoted auth header.
	parts := strings.SplitN(line, " ", 7)
	if len(parts) < 7 {
		return AuditEvent{}, false
	}

	method := parts[1]
	if method != "PUT" && method != "POST" && method != "DELETE" {
		return AuditEvent{}, false
	}

	status, _ := strconv.Atoi(parts[3])
	if status < 200 || status >= 300 {
		return AuditEvent{}, false
	}

	msecF, _ := strconv.ParseFloat(parts[0], 64)
	sec := int64(msecF)
	nsec := int64((msecF - float64(sec)) * 1e9)
	performedAt := time.Unix(sec, nsec).UTC()

	bucket, objectKey := splitBucketKey(parts[2])
	if bucket == "" {
		return AuditEvent{}, false
	}

	var sizeBytes int64
	if method != "DELETE" {
		sizeBytes, _ = strconv.ParseInt(parts[4], 10, 64)
	}

	action := method
	if method == "POST" {
		action = "PUT" // S3 multipart upload completion uses POST
	}

	return AuditEvent{
		Bucket:      bucket,
		ObjectKey:   objectKey,
		Action:      action,
		SizeBytes:   sizeBytes,
		AccessKey:   extractAccessKey(strings.Trim(parts[6], "\"")),
		ClientIP:    parts[5],
		PerformedAt: performedAt,
	}, true
}

// splitBucketKey splits an S3 URI into bucket name and object key.
// "/my-bucket/path/to/obj.txt?v=1" → ("my-bucket", "path/to/obj.txt")
func splitBucketKey(uri string) (bucket, key string) {
	if idx := strings.IndexByte(uri, '?'); idx >= 0 {
		uri = uri[:idx]
	}
	uri = strings.TrimPrefix(uri, "/")
	if slash := strings.IndexByte(uri, '/'); slash >= 0 {
		return uri[:slash], uri[slash+1:]
	}
	return uri, ""
}

// extractAccessKey parses the AWS Sig V4 Credential value from an Authorization
// header and returns the access key (the portion before the first '/').
// Returns empty string if the header is absent or not AWS Sig V4.
func extractAccessKey(auth string) string {
	// "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20260613/us-east-1/..."
	idx := strings.Index(auth, "Credential=")
	if idx < 0 {
		return ""
	}
	rest := auth[idx+len("Credential="):]
	if slash := strings.IndexByte(rest, '/'); slash >= 0 {
		return rest[:slash]
	}
	return rest
}

// PollCluster queries all five components and returns their current state.
func (o *Observer) PollCluster(ctx context.Context) (*ClusterComponents, error) {
	now := time.Now()

	master := o.pollMaster(ctx, now)
	volume := o.pollVolume(ctx, now)
	filer := o.pollFiler(ctx, now)
	s3gw := o.pollS3Gateway(ctx, now)
	nginx := o.pollNginx(ctx, now)

	return &ClusterComponents{
		Master: master,
		Volume: volume,
		Filer:  filer,
		S3:     s3gw,
		Nginx:  nginx,
	}, nil
}

// BucketStats queries the filer REST API for directory entries under /buckets/.
// The filer returns JSON when the Accept: application/json header is set.
func (o *Observer) BucketStats(ctx context.Context) ([]BucketStat, error) {
	// List buckets from filer.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.cfg.FilerURL+"/buckets/", nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying filer buckets: %w", err)
	}
	defer resp.Body.Close()

	var filerResp struct {
		Entries []struct {
			FullPath string `json:"FullPath"`
		} `json:"Entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&filerResp); err != nil {
		return nil, fmt.Errorf("parsing bucket list: %w", err)
	}

	// Aggregate per-collection volume stats from master.
	colStats := o.volumeStatsByCollection(ctx)

	stats := make([]BucketStat, 0, len(filerResp.Entries))
	for _, e := range filerResp.Entries {
		name := e.FullPath
		if len(name) > len("/buckets/") {
			name = name[len("/buckets/"):]
		}
		cs := colStats[name]
		health := "ok"
		if cs.degraded {
			health = "degraded"
		}
		stats = append(stats, BucketStat{
			Name:          name,
			ObjectCount:   cs.objectCount,
			SizeBytes:     cs.sizeBytes,
			ReplicaHealth: health,
		})
	}
	return stats, nil
}

type collectionStat struct {
	objectCount int64
	sizeBytes   int64
	degraded    bool
}

// volumeStatsByCollection queries master /vol/status and aggregates FileCount,
// DeleteCount, and Size by collection name (which equals the S3 bucket name).
func (o *Observer) volumeStatsByCollection(ctx context.Context) map[string]collectionStat {
	body, err := o.getJSON(ctx, o.cfg.MasterURL+"/vol/status")
	if err != nil {
		return nil
	}

	// The topology is nested: Volumes.DataCenters.{dc}.{rack}.{node} = []volumeInfo.
	// Use RawMessage to handle arbitrary depth without knowing node addresses.
	var top struct {
		Volumes struct {
			DataCenters map[string]json.RawMessage `json:"DataCenters"`
		} `json:"Volumes"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return nil
	}

	type volumeInfo struct {
		Collection  string `json:"Collection"`
		Size        int64  `json:"Size"`
		FileCount   int64  `json:"FileCount"`
		DeleteCount int64  `json:"DeleteCount"`
		ReadOnly    bool   `json:"ReadOnly"`
	}

	result := make(map[string]collectionStat)

	for _, rackRaw := range top.Volumes.DataCenters {
		var racks map[string]json.RawMessage
		if err := json.Unmarshal(rackRaw, &racks); err != nil {
			continue
		}
		for _, nodeRaw := range racks {
			var nodes map[string][]volumeInfo
			if err := json.Unmarshal(nodeRaw, &nodes); err != nil {
				continue
			}
			for _, vols := range nodes {
				for _, v := range vols {
					col := v.Collection
					cs := result[col]
					cs.objectCount += v.FileCount - v.DeleteCount
					cs.sizeBytes += v.Size
					if v.ReadOnly {
						cs.degraded = true
					}
					result[col] = cs
				}
			}
		}
	}
	return result
}

// Watch runs a polling loop until ctx is cancelled. It calls onChangeFn
// immediately whenever a significant state change is detected.
func (o *Observer) Watch(ctx context.Context, onChangeFn func(*ClusterComponents, string)) {
	interval := 30 * time.Second
	var prev *ClusterComponents

	poll := func() {
		cur, err := o.PollCluster(ctx)
		if err != nil {
			o.logger.Warn("observer poll failed", zap.Error(err))
			return
		}
		if reason := o.detectChange(prev, cur); reason != "" {
			o.logger.Info("cluster state change detected", zap.String("reason", reason))
			onChangeFn(cur, reason)
		}
		prev = cur
	}

	poll() // immediate poll on start

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			poll()
		}
	}
}

// detectChange returns a non-empty reason string when the new state differs
// significantly from the previous state.
func (o *Observer) detectChange(prev, cur *ClusterComponents) string {
	if prev == nil {
		return "" // first poll — baseline, not a change
	}

	if prev.Master.Reachable != cur.Master.Reachable {
		if cur.Master.Reachable {
			return "master_recovered"
		}
		return "master_unreachable"
	}
	if prev.Master.Service.Active != cur.Master.Service.Active {
		if cur.Master.Service.Active {
			return "weed-master_service_started"
		}
		return "weed-master_service_stopped"
	}

	if prev.Volume.Reachable != cur.Volume.Reachable {
		if cur.Volume.Reachable {
			return "volume_recovered"
		}
		return "volume_unreachable"
	}
	if prev.Volume.Service.Active != cur.Volume.Service.Active {
		if cur.Volume.Service.Active {
			return "weed-volume_service_started"
		}
		return "weed-volume_service_stopped"
	}
	if prev.Volume.DegradedVolumes == 0 && cur.Volume.DegradedVolumes > 0 {
		return fmt.Sprintf("volumes_degraded_%d", cur.Volume.DegradedVolumes)
	}
	if prev.Volume.DegradedVolumes > 0 && cur.Volume.DegradedVolumes == 0 {
		return "volumes_recovered"
	}
	if cur.Volume.CapacityPct >= o.cfg.CapacityCriticalPct && prev.Volume.CapacityPct < o.cfg.CapacityCriticalPct {
		return fmt.Sprintf("capacity_critical_%.1f_pct", cur.Volume.CapacityPct)
	}
	if cur.Volume.CapacityPct >= o.cfg.CapacityWarnPct && prev.Volume.CapacityPct < o.cfg.CapacityWarnPct {
		return fmt.Sprintf("capacity_warn_%.1f_pct", cur.Volume.CapacityPct)
	}

	if prev.Filer.Reachable != cur.Filer.Reachable {
		if cur.Filer.Reachable {
			return "filer_recovered"
		}
		return "filer_unreachable"
	}
	if prev.Filer.Service.Active != cur.Filer.Service.Active {
		if cur.Filer.Service.Active {
			return "weed-filer_service_started"
		}
		return "weed-filer_service_stopped"
	}

	if prev.S3.Reachable != cur.S3.Reachable {
		if cur.S3.Reachable {
			return "s3_gateway_recovered"
		}
		return "s3_gateway_unreachable"
	}
	if prev.S3.Service.Active != cur.S3.Service.Active {
		if cur.S3.Service.Active {
			return "weed-s3_service_started"
		}
		return "weed-s3_service_stopped"
	}

	if prev.Nginx.Service.Active != cur.Nginx.Service.Active {
		if cur.Nginx.Service.Active {
			return "nginx_service_started"
		}
		return "nginx_service_stopped"
	}

	return ""
}

// ---------------------------------------------------------------------------
// Per-component poll helpers
// ---------------------------------------------------------------------------

func (o *Observer) pollMaster(ctx context.Context, now time.Time) MasterComponent {
	svcHealth := o.serviceHealth(ctx, "weed-master")

	var masterResp struct {
		IsLeader bool     `json:"IsLeader"`
		Leader   string   `json:"Leader"`
		Peers    []string `json:"Peers"`
	}

	reachable := false
	isLeader := false
	peers := 0

	body, err := o.getJSON(ctx, o.cfg.MasterURL+"/cluster/status")
	if err == nil {
		if jsonErr := json.Unmarshal(body, &masterResp); jsonErr == nil {
			reachable = true
			isLeader = masterResp.IsLeader
			peers = len(masterResp.Peers)
		}
	}

	return MasterComponent{
		Reachable: reachable,
		IsLeader:  isLeader,
		Peers:     peers,
		CheckedAt: now,
		Service:   svcHealth,
	}
}

func (o *Observer) pollVolume(ctx context.Context, now time.Time) VolumeComponent {
	svcHealth := o.serviceHealth(ctx, "weed-volume")

	var volumeResp struct {
		Version string `json:"Version"`
		Volumes []struct {
			ReadOnly     bool   `json:"readOnly"`
			ReplicaCount int    `json:"replicaCount"`
			Size         uint64 `json:"size"`
			Max          uint64 `json:"max"`
		} `json:"Volumes"`
		DiskStatuses []struct {
			Dir           string  `json:"Dir"`
			Total         uint64  `json:"Total"`
			Used          uint64  `json:"Used"`
			Free          uint64  `json:"Free"`
			PercentFree   float64 `json:"PercentFree"`
			PercentUsed   float64 `json:"PercentUsed"`
		} `json:"DiskStatuses"`
	}

	reachable := false
	var (
		total, writable, degraded, readonly int
		capTotal, capUsed                   uint64
		capPct                              float64
	)

	body, err := o.getJSON(ctx, o.cfg.VolumeURL+"/status")
	if err == nil {
		if jsonErr := json.Unmarshal(body, &volumeResp); jsonErr == nil {
			reachable = true
			total = len(volumeResp.Volumes)
			for _, v := range volumeResp.Volumes {
				if v.ReadOnly {
					readonly++
				} else if v.ReplicaCount < 2 {
					degraded++
				} else {
					writable++
				}
			}
			for _, d := range volumeResp.DiskStatuses {
				capTotal += d.Total
				capUsed += d.Used
			}
			if capTotal > 0 {
				capPct = float64(capUsed) / float64(capTotal) * 100
			}
		}
	}

	return VolumeComponent{
		Reachable:          reachable,
		TotalVolumes:       total,
		WritableVolumes:    writable,
		DegradedVolumes:    degraded,
		ReadOnlyVolumes:    readonly,
		CapacityBytesTotal: capTotal,
		CapacityBytesUsed:  capUsed,
		CapacityPct:        capPct,
		CheckedAt:          now,
		Service:            svcHealth,
	}
}

func (o *Observer) pollFiler(ctx context.Context, now time.Time) FilerComponent {
	svcHealth := o.serviceHealth(ctx, "weed-filer")
	reachable := o.head(ctx, o.cfg.FilerURL+"/")
	return FilerComponent{
		Reachable: reachable,
		CheckedAt: now,
		Service:   svcHealth,
	}
}

func (o *Observer) pollS3Gateway(ctx context.Context, now time.Time) S3GatewayComponent {
	svcHealth := o.serviceHealth(ctx, "weed-s3")

	bucketCount := 0
	reachable := false

	// Probe the S3 gateway with a HEAD request — any HTTP response means it's up.
	// (A 403 means auth is required, which is correct behaviour.)
	req, _ := http.NewRequestWithContext(ctx, http.MethodHead, o.cfg.S3URL+"/", nil)
	if resp, err := o.client.Do(req); err == nil {
		resp.Body.Close()
		reachable = true
		_ = bucketCount // populated by BucketStats via filer
	}

	return S3GatewayComponent{
		Reachable:   reachable,
		BucketCount: bucketCount,
		CheckedAt:   now,
		Service:     svcHealth,
	}
}

func (o *Observer) pollNginx(ctx context.Context, now time.Time) NginxComponent {
	return NginxComponent{
		Service:   o.serviceHealth(ctx, "nginx"),
		CheckedAt: now,
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (o *Observer) serviceHealth(ctx context.Context, name string) ServiceHealth {
	h := ServiceHealth{Name: name}
	if o.svc == nil {
		return h
	}
	info, err := o.svc.Get(ctx, name)
	if err != nil {
		return h
	}
	h.Active = info.State == services.StateActive
	h.SubState = info.SubState
	return h
}

func (o *Observer) getJSON(ctx context.Context, url string) ([]byte, error) {
	return o.getBody(ctx, url)
}

func (o *Observer) getBody(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	return io.ReadAll(resp.Body)
}

func (o *Observer) head(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode < 400
}
