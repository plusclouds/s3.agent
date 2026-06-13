// Package publisher runs the heartbeat, telemetry, and S3 telemetry loops that
// push events from the agent to the platform via the NATS evt subject.
package publisher

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/config"
	s3module "github.com/plusclouds/ubuntu-agent/internal/modules/s3"
	"github.com/plusclouds/ubuntu-agent/internal/modules/system"
	natsclient "github.com/plusclouds/ubuntu-agent/internal/nats"
	"github.com/plusclouds/ubuntu-agent/internal/protocol"
)

const minTelemetryInterval = 1 * time.Second

// Publisher publishes periodic events (heartbeat, telemetry, S3 telemetry)
// to the platform and fires immediate health events via the observer Watch loop.
type Publisher struct {
	nats                *natsclient.Client
	sys                 *system.Module
	s3obs               *s3module.Observer
	agentUUID           string
	agentType           string
	cfg                 config.AgentConfig
	logger              *zap.Logger
	telemetryIntervalCh chan time.Duration
}

// New creates a Publisher.
func New(
	nats *natsclient.Client,
	sys *system.Module,
	s3obs *s3module.Observer,
	agentUUID string,
	agentType string,
	cfg config.AgentConfig,
	logger *zap.Logger,
) *Publisher {
	return &Publisher{
		nats:                nats,
		sys:                 sys,
		s3obs:               s3obs,
		agentUUID:           agentUUID,
		agentType:           agentType,
		cfg:                 cfg,
		logger:              logger,
		telemetryIntervalCh: make(chan time.Duration, 1),
	}
}

// Start launches the heartbeat, telemetry, S3 telemetry, audit, and observer
// Watch goroutines. They run until ctx is cancelled.
func (p *Publisher) Start(ctx context.Context) {
	p.sendCapabilities(ctx) //nolint:errcheck
	go p.heartbeatLoop(ctx)
	go p.telemetryLoop(ctx)
	go p.s3TelemetryLoop(ctx)
	p.s3obs.RunAudit(ctx)
	go p.auditLoop(ctx)
	go p.s3obs.Watch(ctx, func(components *s3module.ClusterComponents, reason string) {
		env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeS3Health,
			s3module.S3HealthPayload{
				Components:    *components,
				TriggerReason: reason,
			})
		if err != nil {
			p.logger.Warn("s3_health: could not build envelope", zap.Error(err))
			return
		}
		if err := p.nats.Publish(env); err != nil {
			p.logger.Warn("s3_health: publish failed", zap.Error(err))
			return
		}
		p.logger.Info("s3_health published", zap.String("reason", reason))
	})
}

// SetTelemetryInterval updates the telemetry push interval at runtime.
// The minimum allowed value is 5s; smaller values are clamped.
func (p *Publisher) SetTelemetryInterval(d time.Duration) time.Duration {
	if d < minTelemetryInterval {
		d = minTelemetryInterval
	}
	select {
	case p.telemetryIntervalCh <- d:
	default:
		<-p.telemetryIntervalCh
		p.telemetryIntervalCh <- d
	}
	return d
}

func (p *Publisher) heartbeatLoop(ctx context.Context) {
	interval := p.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p.sendHeartbeat(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sendHeartbeat(ctx)
		}
	}
}

func (p *Publisher) telemetryLoop(ctx context.Context) {
	interval := p.cfg.TelemetryInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p.sendTelemetry(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case newInterval := <-p.telemetryIntervalCh:
			ticker.Reset(newInterval)
			p.logger.Info("telemetry interval updated", zap.Duration("interval", newInterval))
			p.sendTelemetry(ctx)
		case <-ticker.C:
			p.sendTelemetry(ctx)
		}
	}
}

func (p *Publisher) s3TelemetryLoop(ctx context.Context) {
	if p.s3obs == nil {
		return
	}
	interval := p.cfg.TelemetryInterval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	p.sendS3Telemetry(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.sendS3Telemetry(ctx)
		}
	}
}

// auditLoop drains AuditEvents from the observer and publishes them immediately
// as TypeS3Audit envelopes. Events arriving together in the same poll window are
// batched into one envelope to reduce NATS message count.
func (p *Publisher) auditLoop(ctx context.Context) {
	if p.s3obs == nil {
		return
	}
	evts := p.s3obs.AuditEvents()
	for {
		select {
		case <-ctx.Done():
			return
		case evt := <-evts:
			// Drain all events already queued to form a single batch.
			batch := []s3module.AuditEvent{evt}
		drain:
			for {
				select {
				case e := <-evts:
					batch = append(batch, e)
				default:
					break drain
				}
			}
			env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeS3Audit,
				s3module.S3AuditPayload{Events: batch})
			if err != nil {
				p.logger.Warn("s3_audit: build envelope failed", zap.Error(err))
				continue
			}
			if err := p.nats.Publish(env); err != nil {
				p.logger.Warn("s3_audit: publish failed", zap.Error(err))
				continue
			}
			p.logger.Info("s3_audit published", zap.Int("events", len(batch)))
		}
	}
}

func (p *Publisher) sendHeartbeat(ctx context.Context) {
	info, err := p.sys.GetInfo(ctx)
	if err != nil {
		p.logger.Warn("heartbeat: could not get system info", zap.Error(err))
		return
	}
	env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeHeartbeat, protocol.HeartbeatPayload{
		Version: config.AgentVersion,
		UptimeS: info.Uptime,
	})
	if err != nil {
		p.logger.Warn("heartbeat: could not build envelope", zap.Error(err))
		return
	}
	if err := p.nats.Publish(env); err != nil {
		p.logger.Warn("heartbeat: publish failed", zap.Error(err))
		return
	}
	p.logger.Debug("heartbeat published")
}

func (p *Publisher) sendTelemetry(ctx context.Context) {
	metrics, err := p.sys.GetMetrics(ctx)
	if err != nil {
		p.logger.Warn("telemetry: could not get metrics", zap.Error(err))
		return
	}
	env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeTelemetry, metrics)
	if err != nil {
		p.logger.Warn("telemetry: could not build envelope", zap.Error(err))
		return
	}
	if err := p.nats.Publish(env); err != nil {
		p.logger.Warn("telemetry: publish to evt failed", zap.Error(err))
		return
	}
	p.logger.Info("telemetry published")
}

func (p *Publisher) sendS3Telemetry(ctx context.Context) {
	components, err := p.s3obs.PollCluster(ctx)
	if err != nil {
		p.logger.Warn("s3_telemetry: could not poll cluster", zap.Error(err))
		return
	}
	buckets, err := p.s3obs.BucketStats(ctx)
	if err != nil {
		p.logger.Warn("s3_telemetry: could not get bucket stats", zap.Error(err))
		buckets = nil // publish with empty bucket list rather than skip entirely
	}
	payload := s3module.S3TelemetryPayload{
		Components: *components,
		Buckets:    buckets,
		Traffic:    p.s3obs.FlushTraffic(),
	}
	env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeS3Telemetry, payload)
	if err != nil {
		p.logger.Warn("s3_telemetry: could not build envelope", zap.Error(err))
		return
	}
	if err := p.nats.Publish(env); err != nil {
		p.logger.Warn("s3_telemetry: publish failed", zap.Error(err))
		return
	}
	p.logger.Info("s3_telemetry published")
}

// operationCatalog is the full schema for every supported operation.
// Only operations present in AllowedOperations are published in capabilities.
var operationCatalog = map[string]protocol.OperationSchema{
	"agent.allowed_operations": {
		Operation:   "agent.allowed_operations",
		Description: "Return and re-publish the current capabilities list.",
	},
	"services.list": {
		Operation:   "services.list",
		Description: "List all loaded systemd services on the machine.",
	},
	"services.get": {
		Operation: "services.get",
		Description: "Get detailed status of a single systemd service.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name (e.g. weed-master)."},
		},
	},
	"services.start": {
		Operation: "services.start",
		Description: "Start a stopped systemd service.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to start."},
		},
	},
	"services.stop": {
		Operation: "services.stop",
		Description: "Stop a running systemd service.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to stop."},
		},
	},
	"services.restart": {
		Operation: "services.restart",
		Description: "Restart a systemd service.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to restart."},
		},
	},
	"services.reload": {
		Operation: "services.reload",
		Description: "Send a reload signal to a running service.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to reload."},
		},
	},
	"services.enable": {
		Operation: "services.enable",
		Description: "Enable a service to start automatically on boot.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to enable."},
		},
	},
	"services.disable": {
		Operation: "services.disable",
		Description: "Disable a service from starting automatically on boot.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Service name to disable."},
		},
	},
	"system.info": {
		Operation:   "system.info",
		Description: "Return static system information (hostname, OS, kernel, uptime).",
	},
	"system.metrics": {
		Operation:   "system.metrics",
		Description: "Return a full resource snapshot (CPU, memory, disk, network).",
	},
	"system.cpu": {
		Operation:   "system.cpu",
		Description: "Return CPU usage, core count, load averages, and per-core usage.",
	},
	"system.memory": {
		Operation:   "system.memory",
		Description: "Return RAM utilisation.",
	},
	"system.disk": {
		Operation:   "system.disk",
		Description: "Return disk usage and I/O stats for all real block-device partitions.",
	},
	"system.network": {
		Operation:   "system.network",
		Description: "Return network I/O counters for all physical interfaces.",
	},
	"system.update": {
		Operation:   "system.update",
		Description: "Run apt-get update && apt-get upgrade -y. Ubuntu/Debian only.",
	},
	"telemetry.set_interval": {
		Operation:   "telemetry.set_interval",
		Description: "Change the telemetry push interval at runtime. Minimum 5 seconds.",
		Params: []protocol.OperationParam{
			{Name: "interval_s", Type: "integer", Required: true,
				Description: "New telemetry interval in seconds.", MinValue: intPtr(1)},
		},
	},
	"vm.reboot": {
		Operation:   "vm.reboot",
		Description: "Reboot the machine immediately.",
	},
	"vm.shutdown": {
		Operation:   "vm.shutdown",
		Description: "Shut down the machine immediately.",
	},
	"exec": {
		Operation:   "exec",
		Description: "Execute an allowed binary on the machine.",
		Params: []protocol.OperationParam{
			{Name: "command", Type: "string", Required: true, Description: "Absolute path of the binary to run."},
			{Name: "args", Type: "array", Required: false, Description: "Arguments to pass to the binary."},
		},
	},
	// S3 operations
	"full_sync": {
		Operation:   "full_sync",
		Description: "Apply the complete desired state (buckets + IAM users) from orchestration.",
	},
	"bucket_create": {
		Operation:   "bucket_create",
		Description: "Create a new S3 bucket on the SeaweedFS gateway.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Bucket name."},
			{Name: "bucket_id", Type: "string", Required: false, Description: "Orchestration bucket ID."},
			{Name: "owner_tenant_id", Type: "string", Required: false, Description: "Owning tenant ID."},
			{Name: "replication_factor", Type: "integer", Required: false, Description: "Replication factor."},
		},
	},
	"bucket_delete": {
		Operation:   "bucket_delete",
		Description: "Delete an S3 bucket from the SeaweedFS gateway.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Bucket name to delete."},
			{Name: "force_empty", Type: "string", Required: false, Description: "Set true to delete all objects first."},
		},
	},
	"iam_create": {
		Operation:   "iam_create",
		Description: "Create an IAM identity in s3.json and reload the S3 gateway.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Identity name."},
			{Name: "access_key", Type: "string", Required: true, Description: "AWS-style access key."},
			{Name: "secret_key", Type: "string", Required: true, Description: "Secret key (never logged or returned)."},
		},
	},
	"iam_delete": {
		Operation:   "iam_delete",
		Description: "Remove an IAM identity from s3.json and reload the S3 gateway.",
		Params: []protocol.OperationParam{
			{Name: "name", Type: "string", Required: true, Description: "Identity name to remove."},
		},
	},
	"reconcile": {
		Operation:   "reconcile",
		Description: "Re-run reconcile against the last received full_sync desired state.",
		Params: []protocol.OperationParam{
			{Name: "scope", Type: "string", Required: false, Description: "\"buckets\", \"iam\", or \"all\" (default)."},
		},
	},
	"s3.cluster.status": {
		Operation:   "s3.cluster.status",
		Description: "Return the full SeaweedFS cluster and service health status.",
	},
	"s3.bucket.stats": {
		Operation:   "s3.bucket.stats",
		Description: "Return per-bucket object count and size from the filer API.",
	},
	"s3.iam.list": {
		Operation:   "s3.iam.list",
		Description: "List IAM identities from s3.json (secret keys redacted).",
	},
	"s3.customer.block": {
		Operation:   "s3.customer.block",
		Description: "Block a customer access key at the Nginx level and reload Nginx.",
		Params: []protocol.OperationParam{
			{Name: "access_key", Type: "string", Required: true, Description: "Access key to block."},
			{Name: "reason", Type: "string", Required: false, Description: "Reason for blocking (written into config comment)."},
		},
	},
	"s3.customer.unblock": {
		Operation:   "s3.customer.unblock",
		Description: "Remove a customer access key block and reload Nginx.",
		Params: []protocol.OperationParam{
			{Name: "access_key", Type: "string", Required: true, Description: "Access key to unblock."},
		},
	},
	"s3.blocked.list": {
		Operation:   "s3.blocked.list",
		Description: "List all currently blocked access keys from s3_blocked_keys.conf.",
	},
}

func intPtr(v int) *int { return &v }

// SendCapabilities re-publishes the capabilities event and returns the payload.
func (p *Publisher) SendCapabilities(ctx context.Context) (*protocol.CapabilitiesPayload, error) {
	payload := p.sendCapabilities(ctx)
	return payload, nil
}

func (p *Publisher) sendCapabilities(_ context.Context) *protocol.CapabilitiesPayload {
	p.logger.Info("capabilities: building from config",
		zap.Int("allowed_operations_in_config", len(p.cfg.AllowedOperations)),
	)

	source := p.cfg.AllowedOperations
	if len(source) == 0 {
		p.logger.Warn("capabilities: allowed_operations is empty — publishing full catalog as fallback")
		for op := range operationCatalog {
			source = append(source, op)
		}
	}

	schemas := make([]protocol.OperationSchema, 0, len(source))
	execAllowed := false
	for _, op := range source {
		if op == "exec" {
			execAllowed = true
		}
		if schema, ok := operationCatalog[op]; ok {
			schemas = append(schemas, schema)
		}
	}

	payload := protocol.CapabilitiesPayload{Operations: schemas}
	if execAllowed {
		payload.ExecCommands = p.cfg.AllowedCommands
	}

	env, err := protocol.New(p.agentUUID, p.agentType, protocol.TypeCapabilities, payload)
	if err != nil {
		p.logger.Error("capabilities: could not build envelope", zap.Error(err))
		return &payload
	}
	if err := p.nats.Publish(env); err != nil {
		p.logger.Error("capabilities: publish failed", zap.Error(err))
		return &payload
	}
	p.logger.Info("capabilities published", zap.Int("operation_count", len(schemas)))
	return &payload
}
