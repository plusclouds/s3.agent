// Package dispatcher routes inbound command envelopes to module operations
// and returns result envelopes. Operations must be listed in the agent config's
// allowed_operations; unlisted or unknown operations are rejected without execution.
package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/executor"
	s3module "github.com/plusclouds/ubuntu-agent/internal/modules/s3"
	"github.com/plusclouds/ubuntu-agent/internal/modules/services"
	"github.com/plusclouds/ubuntu-agent/internal/modules/system"
	"github.com/plusclouds/ubuntu-agent/internal/protocol"
	"github.com/plusclouds/ubuntu-agent/internal/publisher"
)

// Dispatcher routes command envelopes to the appropriate module method.
type Dispatcher struct {
	sys             *system.Module
	svc             services.Manager
	exec            *executor.Executor
	s3mgr           *s3module.Manager
	s3obs           *s3module.Observer
	pub             *publisher.Publisher
	allowedOps      map[string]bool
	allowedCommands []string
	agentUUID       string
	agentType       string
	logger          *zap.Logger

	// lastDesired holds the most recent full_sync payload for use by reconcile.
	lastDesired *s3module.FullSyncPayload
}

// New creates a Dispatcher.
func New(
	sys *system.Module,
	svc services.Manager,
	exec *executor.Executor,
	s3mgr *s3module.Manager,
	s3obs *s3module.Observer,
	pub *publisher.Publisher,
	agentUUID string,
	agentType string,
	allowedOps []string,
	allowedCommands []string,
	logger *zap.Logger,
) *Dispatcher {
	ops := make(map[string]bool, len(allowedOps))
	for _, op := range allowedOps {
		ops[op] = true
	}
	return &Dispatcher{
		sys:             sys,
		svc:             svc,
		exec:            exec,
		s3mgr:           s3mgr,
		s3obs:           s3obs,
		pub:             pub,
		allowedOps:      ops,
		allowedCommands: allowedCommands,
		agentUUID:       agentUUID,
		agentType:       agentType,
		logger:          logger,
	}
}

// params is the common command params shape. Fields are optional depending on operation.
type params struct {
	// Generic
	Name      string   `json:"name"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
	IntervalS int      `json:"interval_s"`

	// S3 bucket
	BucketID          string                  `json:"bucket_id"`
	OwnerTenantID     string                  `json:"owner_tenant_id"`
	ReplicationFactor int                     `json:"replication_factor"`
	LifecycleRules    []s3module.LifecycleRule `json:"lifecycle_rules"`
	ForceEmpty        bool                    `json:"force_empty"`

	// S3 IAM
	AccessKey  string            `json:"access_key"`
	SecretKey  string            `json:"secret_key"`
	BucketACLs []s3module.BucketACL `json:"bucket_acls"`

	// Nginx blocking
	Reason string `json:"reason"`

	// Reconcile scope
	Scope string `json:"scope"`
}

// Dispatch handles a single command envelope and returns the result envelope.
// It always returns a valid result — errors become failed/rejected statuses.
func (d *Dispatcher) Dispatch(ctx context.Context, env protocol.Envelope) protocol.Envelope {
	start := time.Now()

	var cmd protocol.CommandPayload
	if err := env.DecodePayload(&cmd); err != nil {
		d.logger.Error("→ command received: could not decode payload",
			zap.String("command_id", env.ID),
			zap.Error(err),
		)
		return d.reject(env, "could not decode command payload: "+err.Error())
	}

	op := cmd.Operation

	d.logger.Info("→ command received",
		zap.String("command_id", env.ID),
		zap.String("operation", op),
	)
	d.logger.Debug("→ command params",
		zap.String("command_id", env.ID),
		zap.String("operation", op),
		zap.ByteString("params", cmd.Params),
	)

	if !d.allowedOps[op] {
		result := d.reject(env, fmt.Sprintf("operation %q is not permitted on this agent", op))
		d.logResult(env.ID, op, protocol.StatusRejected, "operation not in allowed_operations", time.Since(start))
		return result
	}

	// full_sync uses the raw params directly as its payload; other ops use the
	// standard params struct.
	if op == "full_sync" {
		output, err := d.runFullSync(ctx, cmd.Params)
		if err != nil {
			result := d.fail(env, err.Error())
			d.logResult(env.ID, op, protocol.StatusFailed, err.Error(), time.Since(start))
			return result
		}
		result := d.ok(env, output)
		d.logResult(env.ID, op, protocol.StatusCompleted, "", time.Since(start))
		return result
	}

	var p params
	if len(cmd.Params) > 0 && !emptyParams(cmd.Params) {
		if err := json.Unmarshal(cmd.Params, &p); err != nil {
			result := d.reject(env, "could not decode params: "+err.Error())
			d.logResult(env.ID, op, protocol.StatusRejected, "could not decode params: "+err.Error(), time.Since(start))
			return result
		}
	}

	output, err := d.run(ctx, op, p)
	if err != nil {
		result := d.fail(env, err.Error())
		d.logResult(env.ID, op, protocol.StatusFailed, err.Error(), time.Since(start))
		return result
	}

	d.logger.Debug("← command output",
		zap.String("command_id", env.ID),
		zap.String("operation", op),
		zap.Any("output", output),
	)
	result := d.ok(env, output)
	d.logResult(env.ID, op, protocol.StatusCompleted, "", time.Since(start))
	return result
}

// logResult emits the summary log line for a completed dispatch.
func (d *Dispatcher) logResult(commandID, op, status, msg string, elapsed time.Duration) {
	fields := []zap.Field{
		zap.String("command_id", commandID),
		zap.String("operation", op),
		zap.String("status", status),
		zap.Duration("elapsed", elapsed),
	}
	if msg != "" {
		fields = append(fields, zap.String("message", msg))
	}
	switch status {
	case protocol.StatusCompleted:
		d.logger.Info("← result: completed", fields...)
	case protocol.StatusFailed:
		d.logger.Error("← result: failed", fields...)
	case protocol.StatusRejected:
		d.logger.Warn("← result: rejected", fields...)
	}
}

// runFullSync handles the full_sync operation separately because its params
// are decoded directly as FullSyncPayload rather than the generic params struct.
func (d *Dispatcher) runFullSync(ctx context.Context, raw json.RawMessage) (any, error) {
	var payload s3module.FullSyncPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decoding full_sync payload: %w", err)
	}
	d.lastDesired = &payload
	return d.s3mgr.Reconcile(ctx, payload)
}

// run executes the operation and returns the output or an error.
func (d *Dispatcher) run(ctx context.Context, op string, p params) (any, error) {
	switch op {

	// ---- system -------------------------------------------------------
	case "system.info":
		return d.sys.GetInfo(ctx)
	case "system.metrics":
		return d.sys.GetMetrics(ctx)
	case "system.cpu":
		return d.sys.GetCPU(ctx)
	case "system.memory":
		return d.sys.GetMemory(ctx)
	case "system.disk":
		return d.sys.GetDisk(ctx)
	case "system.network":
		return d.sys.GetNetwork(ctx)

	// ---- services -----------------------------------------------------
	case "services.list":
		return d.svc.List(ctx)
	case "services.get":
		return d.svc.Get(ctx, p.Name)
	case "services.start":
		return d.svc.Start(ctx, p.Name)
	case "services.stop":
		return d.svc.Stop(ctx, p.Name)
	case "services.restart":
		return d.svc.Restart(ctx, p.Name)
	case "services.reload":
		return d.svc.Reload(ctx, p.Name)
	case "services.enable":
		return d.svc.Enable(ctx, p.Name)
	case "services.disable":
		return d.svc.Disable(ctx, p.Name)

	// ---- vm -----------------------------------------------------------
	case "vm.reboot":
		var stdout, stderr string
		var execErr error
		if runtime.GOOS == "windows" {
			stdout, stderr, execErr = d.exec.Execute(ctx, "shutdown", "/r", "/t", "0")
		} else {
			stdout, stderr, execErr = d.exec.Execute(ctx, "systemctl", "reboot")
		}
		return map[string]string{"stdout": stdout, "stderr": stderr}, execErr

	case "vm.shutdown":
		var stdout, stderr string
		var execErr error
		if runtime.GOOS == "windows" {
			stdout, stderr, execErr = d.exec.Execute(ctx, "shutdown", "/s", "/t", "0")
		} else {
			stdout, stderr, execErr = d.exec.Execute(ctx, "systemctl", "poweroff")
		}
		return map[string]string{"stdout": stdout, "stderr": stderr}, execErr

	// ---- system.update ------------------------------------------------
	case "system.update":
		distro, err := detectDebianDistro()
		if err != nil {
			return nil, fmt.Errorf("could not detect OS: %w", err)
		}
		if distro == "" {
			return nil, fmt.Errorf("system.update is only supported on Ubuntu/Debian")
		}
		updateOut, updateErr, err := d.exec.Execute(ctx,
			"apt-get", "-y", "-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold", "update")
		if err != nil {
			return map[string]any{"step": "apt-get update", "stdout": updateOut, "stderr": updateErr},
				fmt.Errorf("apt-get update failed: %w", err)
		}
		upgradeOut, upgradeErr, err := d.exec.Execute(ctx,
			"apt-get", "-y", "-o", "Dpkg::Options::=--force-confdef",
			"-o", "Dpkg::Options::=--force-confold", "upgrade")
		return map[string]any{
			"distro": distro, "update_stdout": updateOut, "update_stderr": updateErr,
			"upgrade_stdout": upgradeOut, "upgrade_stderr": upgradeErr,
		}, err

	// ---- agent --------------------------------------------------------
	case "agent.allowed_operations":
		payload, err := d.pub.SendCapabilities(ctx)
		if err != nil {
			return nil, fmt.Errorf("re-publishing capabilities: %w", err)
		}
		return payload, nil

	// ---- telemetry ----------------------------------------------------
	case "telemetry.set_interval":
		if p.IntervalS <= 0 {
			return nil, fmt.Errorf("interval_s must be a positive integer (seconds)")
		}
		requested := time.Duration(p.IntervalS) * time.Second
		applied := d.pub.SetTelemetryInterval(requested)
		return map[string]any{
			"requested_interval_s": p.IntervalS,
			"applied_interval_s":   int(applied.Seconds()),
		}, nil

	// ---- exec ---------------------------------------------------------
	case "exec":
		if !slices.Contains(d.allowedCommands, p.Command) {
			return nil, fmt.Errorf("command %q is not in allowed_commands", p.Command)
		}
		stdout, stderr, err := d.exec.Execute(ctx, p.Command, p.Args...)
		return map[string]string{"stdout": stdout, "stderr": stderr}, err

	// ---- S3: cluster observation --------------------------------------
	case "s3.cluster.status":
		return d.s3obs.PollCluster(ctx)

	case "s3.bucket.stats":
		return d.s3obs.BucketStats(ctx)

	// ---- S3: bucket management ----------------------------------------
	case "bucket_create":
		spec := s3module.BucketSpec{
			BucketID:          p.BucketID,
			Name:              p.Name,
			OwnerTenantID:     p.OwnerTenantID,
			ReplicationFactor: p.ReplicationFactor,
			LifecycleRules:    p.LifecycleRules,
		}
		if err := d.s3mgr.CreateBucket(ctx, spec); err != nil {
			return nil, err
		}
		return map[string]string{"bucket": p.Name, "status": "created"}, nil

	case "bucket_delete":
		if err := d.s3mgr.DeleteBucket(ctx, p.Name, p.ForceEmpty); err != nil {
			return nil, err
		}
		return map[string]string{"bucket": p.Name, "status": "deleted"}, nil

	// ---- S3: IAM management -------------------------------------------
	case "iam_create":
		user := s3module.IAMUser{
			Name:       p.Name,
			AccessKey:  p.AccessKey,
			BucketACLs: p.BucketACLs,
		}
		if err := d.s3mgr.CreateIAMUser(user, p.SecretKey); err != nil {
			return nil, err
		}
		return map[string]string{"name": p.Name, "status": "created"}, nil

	case "iam_delete":
		if err := d.s3mgr.DeleteIAMUser(p.Name); err != nil {
			return nil, err
		}
		return map[string]string{"name": p.Name, "status": "deleted"}, nil

	case "s3.iam.list":
		return d.s3mgr.ListIAM()

	// ---- S3: reconcile ------------------------------------------------
	case "reconcile":
		if d.lastDesired == nil {
			return nil, fmt.Errorf("no desired state available — send full_sync first")
		}
		desired := *d.lastDesired
		if p.Scope == "buckets" {
			desired.IAMUsers = nil
		} else if p.Scope == "iam" {
			desired.Buckets = nil
		}
		return d.s3mgr.Reconcile(ctx, desired)

	// ---- S3: Nginx customer blocking ----------------------------------
	case "s3.customer.block":
		if err := d.s3mgr.BlockAccessKey(p.AccessKey, p.Reason); err != nil {
			return nil, err
		}
		return map[string]string{"access_key": p.AccessKey, "status": "blocked"}, nil

	case "s3.customer.unblock":
		if err := d.s3mgr.UnblockAccessKey(p.AccessKey); err != nil {
			return nil, err
		}
		return map[string]string{"access_key": p.AccessKey, "status": "unblocked"}, nil

	case "s3.blocked.list":
		return d.s3mgr.ListBlocked()

	default:
		return nil, fmt.Errorf("unknown operation %q", op)
	}
}

// --- result helpers ----------------------------------------------------------

func (d *Dispatcher) ok(src protocol.Envelope, output any) protocol.Envelope {
	env, err := protocol.ResultFor(src, d.agentUUID, d.agentType, protocol.StatusCompleted, "", output)
	if err != nil {
		d.logger.Error("failed to build result envelope", zap.Error(err))
	}
	return env
}

func (d *Dispatcher) fail(src protocol.Envelope, msg string) protocol.Envelope {
	env, err := protocol.ResultFor(src, d.agentUUID, d.agentType, protocol.StatusFailed, msg, nil)
	if err != nil {
		d.logger.Error("failed to build result envelope", zap.Error(err))
	}
	return env
}

func (d *Dispatcher) reject(src protocol.Envelope, msg string) protocol.Envelope {
	env, err := protocol.ResultFor(src, d.agentUUID, d.agentType, protocol.StatusRejected, msg, nil)
	if err != nil {
		d.logger.Error("failed to build result envelope", zap.Error(err))
	}
	return env
}

// detectDebianDistro reads /etc/os-release and returns the distro name if the
// system is Ubuntu or Debian. Returns "" for other distros.
func detectDebianDistro() (string, error) {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return "", fmt.Errorf("reading /etc/os-release: %w", err)
	}
	var id, idLike string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "ID=") {
			id = strings.ToLower(strings.Trim(strings.TrimPrefix(line, "ID="), `"`))
		}
		if strings.HasPrefix(line, "ID_LIKE=") {
			idLike = strings.ToLower(strings.Trim(strings.TrimPrefix(line, "ID_LIKE="), `"`))
		}
	}
	if id == "ubuntu" || id == "debian" ||
		strings.Contains(idLike, "ubuntu") || strings.Contains(idLike, "debian") {
		if id != "" {
			return id, nil
		}
		return idLike, nil
	}
	return "", nil
}

// emptyParams reports whether raw JSON represents an absence of params.
func emptyParams(raw []byte) bool {
	switch string(bytes.TrimSpace(raw)) {
	case "null", "[]", "{}", "":
		return true
	}
	return false
}
