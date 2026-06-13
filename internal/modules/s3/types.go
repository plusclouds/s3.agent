// Package s3 provides the SeaweedFS observer and S3 manager subsystems for the
// s3d agent. The observer polls cluster topology and service health; the manager
// executes IAM, Nginx, and bucket operations on behalf of the orchestration platform.
package s3

import "time"

// ---------------------------------------------------------------------------
// Service health
// ---------------------------------------------------------------------------

// ServiceHealth represents the systemd state of a single service unit.
type ServiceHealth struct {
	Name     string `json:"name"`
	Active   bool   `json:"active"`    // true when ActiveState == "active"
	SubState string `json:"sub_state"` // e.g. "running", "dead", "exited"
}

// ---------------------------------------------------------------------------
// Cluster component types (observer output)
// ---------------------------------------------------------------------------

// MasterComponent is the observed state of the SeaweedFS master.
type MasterComponent struct {
	Reachable bool          `json:"reachable"`
	IsLeader  bool          `json:"is_leader"`
	Peers     int           `json:"peers"`
	CheckedAt time.Time     `json:"checked_at"`
	Service   ServiceHealth `json:"service"`
}

// VolumeComponent is the observed state of the SeaweedFS volume server.
type VolumeComponent struct {
	Reachable          bool          `json:"reachable"`
	TotalVolumes       int           `json:"total_volumes"`
	WritableVolumes    int           `json:"writable_volumes"`
	DegradedVolumes    int           `json:"degraded_volumes"`
	ReadOnlyVolumes    int           `json:"readonly_volumes"`
	CapacityBytesTotal uint64        `json:"capacity_bytes_total"`
	CapacityBytesUsed  uint64        `json:"capacity_bytes_used"`
	CapacityPct        float64       `json:"capacity_pct"`
	CheckedAt          time.Time     `json:"checked_at"`
	Service            ServiceHealth `json:"service"`
}

// FilerComponent is the observed state of the SeaweedFS filer.
type FilerComponent struct {
	Reachable bool          `json:"reachable"`
	CheckedAt time.Time     `json:"checked_at"`
	Service   ServiceHealth `json:"service"`
}

// S3GatewayComponent is the observed state of the SeaweedFS S3 gateway.
type S3GatewayComponent struct {
	Reachable   bool          `json:"reachable"`
	BucketCount int           `json:"bucket_count"`
	CheckedAt   time.Time     `json:"checked_at"`
	Service     ServiceHealth `json:"service"`
}

// NginxComponent is the observed state of the Nginx reverse proxy.
type NginxComponent struct {
	Service   ServiceHealth `json:"service"`
	CheckedAt time.Time     `json:"checked_at"`
}

// ClusterComponents holds the full observed state of the storage stack.
type ClusterComponents struct {
	Master MasterComponent    `json:"master"`
	Volume VolumeComponent    `json:"volume"`
	Filer  FilerComponent     `json:"filer"`
	S3     S3GatewayComponent `json:"s3"`
	Nginx  NginxComponent     `json:"nginx"`
}

// ---------------------------------------------------------------------------
// Telemetry payload (TypeS3Telemetry)
// ---------------------------------------------------------------------------

// BucketStat holds per-bucket usage observed via the filer API.
type BucketStat struct {
	Name          string `json:"name"`
	ObjectCount   int64  `json:"object_count"`
	SizeBytes     int64  `json:"size_bytes"`
	ReplicaHealth string `json:"replica_health"` // "ok" | "degraded"
}

// TrafficStat holds per-bucket traffic delta for one reporting window.
// BytesIn counts upload (PUT/POST request body); BytesOut counts download (GET response body).
// Only 2xx responses are counted.
type TrafficStat struct {
	BucketName string `json:"bucket_name"`
	BytesIn    int64  `json:"bytes_in"`  // upload bytes
	BytesOut   int64  `json:"bytes_out"` // download bytes
	Requests   int64  `json:"requests"`  // total 2xx request count
}

// AuditEvent records a single data-mutation operation detected in the Nginx
// audit log. Published immediately (within ~500 ms) via TypeS3Audit.
type AuditEvent struct {
	Bucket      string     `json:"bucket"`
	ObjectKey   string     `json:"object_key"`
	Action      string     `json:"action"`              // "PUT" | "DELETE"
	SizeBytes   int64      `json:"size_bytes"`          // upload bytes; 0 for DELETE
	RetainUntil *time.Time `json:"retain_until,omitempty"` // WORM retention expiry if set
	AccessKey   string     `json:"access_key,omitempty"`   // S3 access key from Authorization header
	ClientIP    string     `json:"client_ip,omitempty"`
	PerformedAt time.Time  `json:"performed_at"`
}

// S3AuditPayload is the envelope payload for TypeS3Audit messages.
// Events is a batch of all mutations detected in the current poll window.
type S3AuditPayload struct {
	Events []AuditEvent `json:"events"`
}

// S3TelemetryPayload is published every telemetry tick as TypeS3Telemetry.
type S3TelemetryPayload struct {
	Components ClusterComponents `json:"components"`
	Buckets    []BucketStat      `json:"buckets"`
	Traffic    []TrafficStat     `json:"traffic"` // delta since last tick
}

// ---------------------------------------------------------------------------
// Health event payload (TypeS3Health)
// ---------------------------------------------------------------------------

// S3HealthPayload is published immediately when a component state change is
// detected. TriggerReason names the specific condition that caused the event.
type S3HealthPayload struct {
	Components    ClusterComponents `json:"components"`
	TriggerReason string            `json:"trigger_reason"`
}

// ---------------------------------------------------------------------------
// S3 manager types
// ---------------------------------------------------------------------------

// LifecycleRule is an S3 object lifecycle policy rule.
type LifecycleRule struct {
	Prefix     string `json:"prefix"`
	ExpireDays int    `json:"expire_days"`
}

// BucketSpec is the desired state of an S3 bucket, as sent by orchestration.
type BucketSpec struct {
	BucketID          string          `json:"bucket_id"`
	Name              string          `json:"name"`
	OwnerTenantID     string          `json:"owner_tenant_id"`
	ReplicationFactor int             `json:"replication_factor"`
	LifecycleRules    []LifecycleRule `json:"lifecycle_rules,omitempty"`
}

// WORMBucketSpec is the desired state of an Object Lock (WORM) bucket.
// Object Lock cannot be disabled once enabled on a bucket.
type WORMBucketSpec struct {
	BucketID      string `json:"bucket_id"`
	Name          string `json:"name"`
	OwnerTenantID string `json:"owner_tenant_id"`
	// Mode is the default retention mode: "COMPLIANCE" or "GOVERNANCE".
	// COMPLIANCE: no one (including admin) can delete objects before retention expires.
	// GOVERNANCE: users with s3:BypassGovernanceRetention can override.
	Mode          string `json:"mode"`
	// RetentionDays is the default retention period in days applied to every object written.
	RetentionDays int    `json:"retention_days"`
}

// BucketACL is an access control entry tying an IAM user to a bucket.
type BucketACL struct {
	BucketID   string `json:"bucket_id"`
	Permission string `json:"permission"` // "r" | "rw" | "admin"
}

// IAMUser is the desired state of an IAM identity, as sent by orchestration.
// SecretKey is inbound-only and never included in response payloads.
type IAMUser struct {
	UserID        string      `json:"user_id"`
	Name          string      `json:"name"`
	OwnerTenantID string      `json:"owner_tenant_id"`
	AccessKey     string      `json:"access_key"`
	SecretKey     string      `json:"secret_key,omitempty"` // inbound only
	BucketACLs    []BucketACL `json:"bucket_acls,omitempty"`
}

// FullSyncPayload is the complete desired state sent by orchestration on every
// NATS connect. The agent reconciles actual state to match this payload.
type FullSyncPayload struct {
	Buckets  []BucketSpec `json:"buckets"`
	IAMUsers []IAMUser    `json:"iam_users"`
}

// ReconcileResult summarises what changed during a reconcile pass.
type ReconcileResult struct {
	BucketsCreated int      `json:"buckets_created"`
	BucketsDeleted int      `json:"buckets_deleted"`
	IAMCreated     int      `json:"iam_created"`
	IAMDeleted     int      `json:"iam_deleted"`
	Errors         []string `json:"errors,omitempty"`
}
