package s3

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/config"
	"github.com/plusclouds/ubuntu-agent/internal/modules/services"
)

// Observer polls the SeaweedFS cluster and systemd services, reporting
// ClusterComponents to callers. It triggers Watch callbacks immediately when
// component state changes are detected.
type Observer struct {
	cfg    config.S3Config
	svc    services.Manager
	client *http.Client
	logger *zap.Logger
}

// NewObserver creates an Observer.
func NewObserver(cfg config.S3Config, svc services.Manager, logger *zap.Logger) *Observer {
	return &Observer{
		cfg:    cfg,
		svc:    svc,
		client: &http.Client{Timeout: 5 * time.Second},
		logger: logger,
	}
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

// BucketStats queries the filer API for per-bucket usage.
func (o *Observer) BucketStats(ctx context.Context) ([]BucketStat, error) {
	url := o.cfg.FilerURL + "/buckets?pretty=y"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}

	resp, err := o.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying filer buckets: %w", err)
	}
	defer resp.Body.Close()

	// The filer returns an S3-style ListAllMyBucketsResult XML.
	var result struct {
		Buckets struct {
			Bucket []struct {
				Name string `xml:"Name"`
			} `xml:"Bucket"`
		} `xml:"Buckets"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing bucket list: %w", err)
	}

	stats := make([]BucketStat, 0, len(result.Buckets.Bucket))
	for _, b := range result.Buckets.Bucket {
		stats = append(stats, BucketStat{
			Name:          b.Name,
			ReplicaHealth: "ok",
		})
	}
	return stats, nil
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

	// List all buckets via the S3 API (no auth needed on localhost admin identity).
	body, err := o.getBody(ctx, o.cfg.S3URL+"/")
	if err == nil {
		reachable = true
		// Count <Bucket> elements without full XML parsing.
		var listResp struct {
			Buckets struct {
				Bucket []struct {
					Name string `xml:"Name"`
				} `xml:"Bucket"`
			} `xml:"Buckets"`
		}
		if xmlErr := xml.Unmarshal(body, &listResp); xmlErr == nil {
			bucketCount = len(listResp.Buckets.Bucket)
		}
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
