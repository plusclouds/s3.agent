# storaged — Design Document

## Overview

`storaged` is a stateless Go agent that runs on a storage server alongside a SeaweedFS cluster. Its sole responsibility is S3 bucket management and SeaweedFS health observation. It receives intent from a central orchestration service and reports actual state back in real time.

VM disk and NFS export management is handled by a separate dedicated agent (`nfsd`) on the same server. The two agents are fully independent — no local IPC, no shared config, no shared state.

---

## System Context

```
┌─────────────────────────────────────────────────────┐
│                 Orchestration service                │
│         VMs · topology · intent store               │
└──────────────┬──────────────────────┬───────────────┘
     SSE push  │                      │  SSE push
  telemetry ▲  │                      │  ▲ telemetry
               ▼                      ▼
┌──────────────────────────────────────────────────────┐
│                    Storage server                    │
│                                                      │
│  ┌─────────────────┐      ┌────────────────────┐    │
│  │   nfsd agent    │      │     storaged        │    │
│  │ NFS exports     │      │ S3 buckets · IAM    │    │
│  │ VM disk volumes │      │ SeaweedFS topology  │    │
│  └────────┬────────┘      └─────────┬──────────┘    │
│           │                         │                │
│  ┌────────▼────────┐      ┌─────────▼──────────┐    │
│  │ Kernel/exportfs │      │  SeaweedFS cluster  │    │
│  └─────────────────┘      │ master·volume·filer │    │
│                            │      ·s3            │    │
│                            └────────────────────┘    │
└──────────────────────────────────────────────────────┘
```

---

## Agent Design Principles

- **Stateless executor** — no local database, no persistent state on disk. All desired state comes from orchestration on connect; all observed state is pushed back immediately.
- **Read-only towards SeaweedFS internals** — `storaged` never calls SeaweedFS repair or rebalance endpoints. It observes and reports; orchestration decides what action to take.
- **PATCH on state change, not on every tick** — the observer triggers a PATCH to orchestration only when a value crosses a threshold or a `reachable` boolean flips. Routine heartbeat data goes on a separate 30-second telemetry tick.
- **In-memory only at runtime** — the agent keeps a short-lived in-memory cache of the last received command batch and a rolling 60-second metric buffer. Nothing is written to disk.

---

## Architecture

### Interface layer

| Component | Role |
|---|---|
| SSE consumer | Opens persistent `GET /v1/agents/{id}/stream`, receives commands from orchestration |
| REST API (gin) | Local CLI interface and health check endpoint |
| Telemetry engine | POSTs observed state to `POST /v1/agents/{id}/telemetry` every 30 seconds |

### Core

The agent core contains the command dispatcher, reconcile loop, job scheduler, ack emitter, and config loader (viper). It has no knowledge of SeaweedFS or S3 internals — it delegates to the two subsystems.

### Subsystems

**S3 manager**
- Bucket create / delete / update
- IAM users, access keys, ACLs
- Lifecycle rules and quota enforcement
- Bucket existence reconcile against orchestration intent
- Talks to SeaweedFS S3 gateway on `:8333` (AWS S3-compatible API)

**SeaweedFS observer**
- Cluster topology polling (master, volume servers, filer, S3 gateway)
- Volume health and replication factor checking
- Master and filer reachability probes
- Disk capacity reporting
- Talks to master API on `:9333` and filer API on `:8888`
- On problem detection: immediately PATCHes `s3_server` object on orchestration

---

## Communication with Orchestration

### Transport and authentication

- **Persistent channel (commands):** HTTP/2 SSE — `GET /v1/agents/{id}/stream`
- **Agent-to-orchestration:** HTTPS POST for telemetry, ack, and state updates
- **Authentication:** `X-Agent-Key: {api_key}` header on every request, TLS required

### Channel overview

```
Orchestration                              storaged
     │                                        │
     │── SSE stream (commands) ──────────────▶│
     │                                        │
     │◀── POST /telemetry (every 30s) ────────│
     │◀── POST /ack (per command) ─────────────│
     │◀── PATCH /s3-servers/{id} (on change) ─│
```

### Connect and bootstrap sequence

1. `storaged` opens SSE stream with `Last-Event-ID` header (empty on first boot).
2. Orchestration sends `full_sync` as the first event — always, regardless of `Last-Event-ID`.
3. `storaged` applies `full_sync` idempotently: create-or-update buckets and IAM, remove anything not in the payload.
4. `storaged` immediately sends a telemetry POST (does not wait for the 30-second tick).
5. Normal operation: SSE stream stays open, telemetry POSTed every 30 seconds.

**Reconnect backoff:** 1s → 2s → 4s → 8s → max 60s, jittered. Resets to 1s after a successful telemetry ack.

**Dead agent detection:** Orchestration marks agent status `unknown` if no telemetry POST is received for more than 90 seconds (3 missed ticks). On reconnect, `full_sync` re-establishes ground truth.

---

## SSE Command Types (Orchestration → storaged)

All SSE messages share a common envelope:

```json
{
  "id":      "evt_01HZ...",
  "type":    "<command_type>",
  "sent_at": "2026-05-26T10:00:00Z",
  "payload": { }
}
```

| Type | Description |
|---|---|
| `full_sync` | Complete desired state — all buckets, IAM, lifecycle rules. Sent on every connect. |
| `bucket_create` | Create a new S3 bucket with owner, replication factor, and lifecycle rules. |
| `bucket_delete` | Delete a bucket. `force_empty: true` required to delete non-empty buckets. |
| `bucket_update` | Update lifecycle rules, replication factor, or ACLs on an existing bucket. |
| `iam_create` | Create an IAM user and access key for a tenant. |
| `iam_delete` | Delete an IAM user and revoke all access keys. |
| `reconcile` | Force immediate reconcile of a given scope (`buckets`, `iam`, or `all`). |

### Example — `full_sync` payload

```json
{
  "buckets": [
    {
      "bucket_id":         "bkt_s3a",
      "name":              "tenant-backups",
      "owner_tenant_id":   "ten_99",
      "replication_factor": 2,
      "lifecycle_rules":   [{ "prefix": "logs/", "expire_days": 30 }]
    }
  ],
  "iam_users": [
    {
      "user_id":        "iam_u01",
      "name":           "tenant-99-rw",
      "owner_tenant_id":"ten_99",
      "access_key":     "AKIAIOSFODNN7EXAMPLE",
      "secret_key":     "wJalrXUtnFEMI...",
      "bucket_acls":    [{ "bucket_id": "bkt_s3a", "permission": "rw" }]
    }
  ]
}
```

---

## Telemetry POST (storaged → Orchestration)

`POST /v1/agents/{id}/telemetry` — every 30 seconds.

```json
{
  "agent_id":       "agt_storage01",
  "reported_at":    "2026-05-26T10:00:30Z",
  "agent_version":  "1.4.2",
  "uptime_seconds": 86412,
  "seaweedfs": {
    "master_reachable": true,
    "volume_count":     48,
    "volumes_degraded": 0,
    "total_bytes":      107374182400
  },
  "buckets": [
    {
      "bucket_id":     "bkt_s3a",
      "object_count":  14200,
      "size_bytes":    5368709120,
      "replica_health":"ok"
    }
  ]
}
```

---

## Ack POST (storaged → Orchestration)

`POST /v1/agents/{id}/ack` — sent immediately after every SSE command is executed.

```json
{
  "agent_id":        "agt_storage01",
  "command_event_id":"evt_01HZ...",
  "command_type":    "bucket_create",
  "acked_at":        "2026-05-26T10:01:02Z",
  "status":          "ok",
  "error":           null,
  "duration_ms":     142
}
```

---

## `s3_server` Object and PATCH Protocol

### Object shape

The `s3_server` object lives in orchestration. `storaged` only writes to `components.*` and `agent_*` fields via PATCH. The top-level `health` and `health_summary` are derived by orchestration from the component sub-objects.

```json
{
  "id":                  "srv_storage01",
  "hostname":            "storage01.internal",
  "agent_version":       "1.4.2",
  "seaweedfs_version":   "3.68",
  "created_at":          "2026-01-10T09:00:00Z",

  "agent_status":        "online",
  "agent_last_seen_at":  "2026-05-26T10:04:30Z",
  "agent_connected_at":  "2026-05-26T08:00:01Z",

  "health":         "degraded",
  "health_summary": "2 volumes under-replicated",

  "components": {
    "master": {
      "reachable":  true,
      "is_leader":  true,
      "peers":      2,
      "checked_at": "2026-05-26T10:04:10Z"
    },
    "volume": {
      "reachable":             true,
      "total_volumes":         48,
      "volumes_writable":      46,
      "volumes_degraded":      2,
      "volumes_readonly":      0,
      "capacity_bytes_total":  107374182400,
      "capacity_bytes_used":   42949672960,
      "capacity_pct":          40.0,
      "checked_at":            "2026-05-26T10:04:12Z"
    },
    "filer": {
      "reachable":  true,
      "checked_at": "2026-05-26T10:04:14Z"
    },
    "s3": {
      "reachable":    true,
      "bucket_count": 14,
      "checked_at":   "2026-05-26T10:04:18Z"
    }
  }
}
```

### PATCH endpoint

```
PATCH /v1/s3-servers/{id}
X-Agent-Key: {api_key}
Content-Type: application/merge-patch+json
```

Uses JSON Merge Patch (RFC 7396). Only changed fields are sent; unmentioned fields are left untouched on the orchestration side.

### PATCH trigger rules

`storaged` triggers a PATCH immediately when:

- A `reachable` boolean on any component flips state (up → down or down → up)
- `volumes_degraded` changes from 0 to any positive value, or returns to 0
- `capacity_pct` crosses a threshold (default: warn at 80%, critical at 90%)
- `volumes_readonly` increases unexpectedly

`storaged` does **not** PATCH on every poll tick for stable values — that is handled by the 30-second telemetry POST.

### Example PATCH payloads

**Volumes degraded:**
```json
{
  "components": {
    "volume": {
      "reachable":         true,
      "total_volumes":     48,
      "volumes_writable":  46,
      "volumes_degraded":  2,
      "volumes_readonly":  0,
      "capacity_bytes_total": 107374182400,
      "capacity_bytes_used":  42949672960,
      "capacity_pct":      40.0,
      "checked_at":        "2026-05-26T10:04:12Z"
    }
  }
}
```

**Master unreachable:**
```json
{
  "components": {
    "master": {
      "reachable":  false,
      "is_leader":  false,
      "peers":      0,
      "checked_at": "2026-05-26T10:04:15Z"
    }
  }
}
```

**Capacity threshold crossed:**
```json
{
  "components": {
    "volume": {
      "reachable":            true,
      "volumes_degraded":     0,
      "capacity_bytes_total": 107374182400,
      "capacity_bytes_used":  92274688000,
      "capacity_pct":         85.9,
      "checked_at":           "2026-05-26T10:04:12Z"
    }
  }
}
```

---

## Data Ownership Summary

### What orchestration owns (intent / desired state)

| Entity | Fields |
|---|---|
| S3 server | `id`, `hostname`, identity fields, `health` (derived) |
| Bucket | `bucket_id`, `name`, `owner_tenant_id`, `replication_factor`, `lifecycle_rules` |
| IAM user | `user_id`, `name`, `owner_tenant_id`, `access_key`, `secret_key`, `bucket_acls` |

### What storaged reports (actual / observed state)

| Entity | Fields |
|---|---|
| Agent | `agent_status`, `agent_last_seen_at`, `agent_connected_at`, `agent_version` |
| Master component | `reachable`, `is_leader`, `peers`, `checked_at` |
| Volume component | `reachable`, `total_volumes`, `volumes_writable`, `volumes_degraded`, `volumes_readonly`, `capacity_*`, `checked_at` |
| Filer component | `reachable`, `checked_at` |
| S3 component | `reachable`, `bucket_count`, `checked_at` |
| Buckets (telemetry) | `object_count`, `size_bytes`, `replica_health` |

### What storaged never stores locally

- Bucket or IAM intent — always re-fetched from `full_sync` on reconnect
- Historical metrics — no local time-series, no local DB
- SeaweedFS internal state — never writes to repair or rebalance endpoints

---

## Automation Model

| Operation | Mode | Trigger |
|---|---|---|
| Bucket reconcile | Auto | On `full_sync` or `reconcile` command |
| IAM reconcile | Auto | On `full_sync` or `reconcile` command |
| Health PATCH to orchestration | Auto (immediate) | On state change detection |
| Telemetry heartbeat | Auto (scheduled) | Every 30 seconds |
| Bucket delete | Manual gate | Requires explicit `bucket_delete` SSE command with `force_empty` flag |
| IAM user delete | Manual gate | Requires explicit `iam_delete` SSE command |

---

## Out of Scope for storaged

The following are explicitly handled by the `nfsd` agent, not `storaged`:

- NFS export creation, deletion, and reconciliation
- VM disk volume tracking and quota enforcement
- Disk rsync / sync jobs for VM data
- Snapshot triggers for VM volumes
- Hypervisor-to-export mapping

There is no communication between `storaged` and `nfsd`. Orchestration is the only system aware of both agents simultaneously.
