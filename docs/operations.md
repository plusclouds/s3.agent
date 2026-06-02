# s3d — Operation Reference

Every command from the platform arrives as a NATS message on `agent.storage.{uuid}.cmd`
and is wrapped in the standard envelope:

```json
{
  "v": 1,
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "command",
  "agent_type": "storage",
  "agent_uuid": "d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp": 1748822400,
  "payload": {
    "operation": "<operation>",
    "params": { }
  }
}
```

The agent replies with a `result` envelope on `agent.storage.{uuid}.evt`
(or directly to `reply_to` when the platform uses synchronous request/reply).

Operations not listed in `allowed_operations` in `agent.yaml` return `status: "rejected"`.

---

## Table of Contents

1. [Agent](#1-agent)
2. [Services](#2-services)
3. [System](#3-system)
4. [Telemetry](#4-telemetry)
5. [S3 Cluster Observation](#5-s3-cluster-observation)
6. [Bucket Management](#6-bucket-management)
7. [IAM Management](#7-iam-management)
8. [Nginx Customer Blocking](#8-nginx-customer-blocking)
9. [Exec](#9-exec)
10. [VM Power](#10-vm-power)

---

## 1. Agent

### `agent.allowed_operations`

Re-publishes the capabilities event and returns the current operation list.

**Params:** none

**Request**
```json
{
  "operation": "agent.allowed_operations"
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "operations": [
      { "operation": "services.list", "description": "List all loaded systemd services on the machine." },
      { "operation": "s3.cluster.status", "description": "Return the full SeaweedFS cluster and service health status." }
    ]
  }
}
```

---

## 2. Services

Manages systemd services on the storage host — including the SeaweedFS components
(`weed-master`, `weed-volume`, `weed-filer`, `weed-s3`) and Nginx.

### `services.list`

Lists all loaded systemd services.

**Params:** none

**Request**
```json
{
  "operation": "services.list"
}
```

**Response**
```json
{
  "status": "completed",
  "output": [
    { "name": "weed-master.service", "state": "active", "sub_state": "running", "enabled": true, "pid": 1234 },
    { "name": "weed-volume.service", "state": "active", "sub_state": "running", "enabled": true, "pid": 1235 },
    { "name": "weed-filer.service",  "state": "active", "sub_state": "running", "enabled": true, "pid": 1236 },
    { "name": "weed-s3.service",     "state": "active", "sub_state": "running", "enabled": true, "pid": 1237 },
    { "name": "nginx.service",       "state": "active", "sub_state": "running", "enabled": true, "pid": 1238 }
  ]
}
```

---

### `services.get`

Gets the status of a single service.

| Param | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Service name — `.service` suffix is optional |

**Request**
```json
{
  "operation": "services.get",
  "params": { "name": "weed-s3" }
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "name": "weed-s3.service",
    "description": "SeaweedFS S3 Gateway",
    "state": "active",
    "sub_state": "running",
    "enabled": true,
    "pid": 1237,
    "since": 1748822000
  }
}
```

---

### `services.start`

Starts a stopped service.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.start",
  "params": { "name": "weed-s3" }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "service": "weed-s3.service", "action": "start", "success": true, "message": "Service weed-s3.service started successfully." }
}
```

---

### `services.stop`

Stops a running service.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.stop",
  "params": { "name": "weed-volume" }
}
```

---

### `services.restart`

Restarts a service (stop then start).

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.restart",
  "params": { "name": "nginx" }
}
```

---

### `services.reload`

Sends a reload signal to a running service without stopping it.
Use for Nginx config reloads or `weed-s3` IAM reloads.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.reload",
  "params": { "name": "weed-s3" }
}
```

---

### `services.enable`

Enables a service to start automatically on boot.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.enable",
  "params": { "name": "weed-master" }
}
```

---

### `services.disable`

Disables a service from starting automatically on boot.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "services.disable",
  "params": { "name": "weed-master" }
}
```

---

## 3. System

### `system.info`

Returns static host information.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "hostname": "storage01.internal",
    "os": "ubuntu 24.04",
    "kernel_version": "6.8.0-124-generic",
    "architecture": "x86_64",
    "uptime": 864012,
    "boot_time": 1748000000
  }
}
```

---

### `system.metrics`

Returns a full resource snapshot (CPU + memory + disk + network).

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "cpu":    { "usage_pct": 12.5, "core_count": 16, "load_avg": [0.45, 0.38, 0.29] },
    "memory": { "total_bytes": 68719476736, "used_bytes": 12884901888, "usage_pct": 18.7 },
    "disks": [
      {
        "device": "/dev/sda1", "mountpoint": "/data/seaweedfs",
        "total_bytes": 10737418240000, "used_bytes": 2147483648000, "usage_pct": 20.0,
        "io": { "read_bytes_per_s": 10485760, "write_bytes_per_s": 5242880, "read_iops": 800, "write_iops": 400, "util_pct": 12.3 }
      }
    ],
    "network": [
      { "interface": "eth0", "bytes_sent": 10737418240, "bytes_recv": 53687091200, "is_up": true }
    ]
  }
}
```

---

### `system.cpu`

Returns CPU usage and per-core breakdown.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "usage_pct": 12.5,
    "core_count": 16,
    "load_avg": [0.45, 0.38, 0.29],
    "cores": [
      { "id": 0, "usage_pct": 18.2 },
      { "id": 1, "usage_pct": 9.7 }
    ]
  }
}
```

---

### `system.memory`

Returns RAM utilisation.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "total_bytes": 68719476736,
    "used_bytes": 12884901888,
    "usage_pct": 18.7
  }
}
```

---

### `system.disk`

Returns disk usage and I/O rates for all real block-device partitions.
Pseudo-filesystems (`tmpfs`, `devtmpfs`, etc.) are excluded.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": [
    {
      "device": "/dev/sda1",
      "mountpoint": "/data/seaweedfs",
      "total_bytes": 10737418240000,
      "used_bytes": 2147483648000,
      "usage_pct": 20.0,
      "io": {
        "read_bytes_per_s": 10485760,
        "write_bytes_per_s": 5242880,
        "read_iops": 800,
        "write_iops": 400,
        "util_pct": 12.3
      }
    }
  ]
}
```

---

### `system.network`

Returns I/O counters for all physical interfaces. Loopback, Docker, and veth interfaces are excluded.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": [
    { "interface": "eth0", "bytes_sent": 10737418240, "bytes_recv": 53687091200, "is_up": true },
    { "interface": "eth1", "bytes_sent": 1073741824,  "bytes_recv": 5368709120,  "is_up": true }
  ]
}
```

---

### `system.update`

Runs `apt-get update && apt-get upgrade -y`. Ubuntu/Debian only.
This operation blocks until completion and may take several minutes.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "distro": "ubuntu",
    "update_stdout": "Hit:1 http://archive.ubuntu.com/ubuntu noble InRelease\n...",
    "update_stderr": "",
    "upgrade_stdout": "Reading package lists...\n0 upgraded, 0 newly installed...",
    "upgrade_stderr": ""
  }
}
```

---

## 4. Telemetry

### `telemetry.set_interval`

Changes the telemetry push interval at runtime. Takes effect immediately — an
extra snapshot is published right away so the platform doesn't wait for the next tick.

| Param | Type | Required | Minimum |
|---|---|---|---|
| `interval_s` | integer | yes | 1 |

**Request**
```json
{
  "operation": "telemetry.set_interval",
  "params": { "interval_s": 10 }
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "requested_interval_s": 10,
    "applied_interval_s": 10
  }
}
```

---

## 5. S3 Cluster Observation

### `s3.cluster.status`

Returns the full observed state of all five stack components — systemd service health
and HTTP API reachability — in a single call.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": {
    "master": {
      "reachable": true,
      "is_leader": true,
      "peers": 0,
      "checked_at": "2026-06-02T10:00:00Z",
      "service": { "name": "weed-master", "active": true, "sub_state": "running" }
    },
    "volume": {
      "reachable": true,
      "total_volumes": 48,
      "writable_volumes": 46,
      "degraded_volumes": 2,
      "readonly_volumes": 0,
      "capacity_bytes_total": 10737418240000,
      "capacity_bytes_used": 2147483648000,
      "capacity_pct": 20.0,
      "checked_at": "2026-06-02T10:00:01Z",
      "service": { "name": "weed-volume", "active": true, "sub_state": "running" }
    },
    "filer": {
      "reachable": true,
      "checked_at": "2026-06-02T10:00:02Z",
      "service": { "name": "weed-filer", "active": true, "sub_state": "running" }
    },
    "s3": {
      "reachable": true,
      "bucket_count": 14,
      "checked_at": "2026-06-02T10:00:03Z",
      "service": { "name": "weed-s3", "active": true, "sub_state": "running" }
    },
    "nginx": {
      "checked_at": "2026-06-02T10:00:03Z",
      "service": { "name": "nginx", "active": true, "sub_state": "running" }
    }
  }
}
```

---

### `s3.bucket.stats`

Returns per-bucket object count and size from the filer API.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": [
    { "name": "tenant-a-backups",  "object_count": 14200, "size_bytes": 5368709120, "replica_health": "ok" },
    { "name": "tenant-a-uploads",  "object_count": 320,   "size_bytes": 107374182,  "replica_health": "ok" },
    { "name": "tenant-b-archives", "object_count": 8800,  "size_bytes": 2147483648, "replica_health": "degraded" }
  ]
}
```

---

## 6. Bucket Management

### `full_sync`

Applies the complete desired state from orchestration. The agent diffs the desired
payload against live state and performs the minimum set of creates and deletes.
Orchestration sends this on every agent reconnect to re-establish ground truth.

| Param | Type | Required | Description |
|---|---|---|---|
| `buckets` | array | yes | Desired bucket list |
| `buckets[].bucket_id` | string | yes | Orchestration-internal bucket ID |
| `buckets[].name` | string | yes | S3 bucket name |
| `buckets[].owner_tenant_id` | string | yes | Owning tenant ID |
| `buckets[].replication_factor` | integer | no | Defaults to SeaweedFS cluster default |
| `buckets[].lifecycle_rules` | array | no | Object lifecycle rules |
| `iam_users` | array | yes | Desired IAM identity list |
| `iam_users[].user_id` | string | yes | Orchestration-internal user ID |
| `iam_users[].name` | string | yes | Identity name in s3.json |
| `iam_users[].access_key` | string | yes | AWS-style access key |
| `iam_users[].secret_key` | string | yes* | Required only for new users (absent = existing user, no rotation) |
| `iam_users[].bucket_acls` | array | no | Per-bucket ACL entries |

**Request**
```json
{
  "operation": "full_sync",
  "params": {
    "buckets": [
      {
        "bucket_id": "bkt_001",
        "name": "tenant-a-backups",
        "owner_tenant_id": "ten_a",
        "replication_factor": 2,
        "lifecycle_rules": [
          { "prefix": "logs/", "expire_days": 30 }
        ]
      },
      {
        "bucket_id": "bkt_002",
        "name": "tenant-b-archives",
        "owner_tenant_id": "ten_b",
        "replication_factor": 1
      }
    ],
    "iam_users": [
      {
        "user_id": "iam_001",
        "name": "tenant-a",
        "owner_tenant_id": "ten_a",
        "access_key": "AKIAIOSFODNN7TENANTA",
        "secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
        "bucket_acls": [
          { "bucket_id": "bkt_001", "permission": "rw" }
        ]
      },
      {
        "user_id": "iam_002",
        "name": "tenant-b",
        "owner_tenant_id": "ten_b",
        "access_key": "AKIAIOSFODNN7TENANTB",
        "secret_key": "anotherSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyz1",
        "bucket_acls": [
          { "bucket_id": "bkt_002", "permission": "admin" }
        ]
      }
    ]
  }
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "buckets_created": 1,
    "buckets_deleted": 0,
    "iam_created": 1,
    "iam_deleted": 0,
    "errors": []
  }
}
```

---

### `bucket_create`

Creates a single S3 bucket on the SeaweedFS gateway.

| Param | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Bucket name |
| `bucket_id` | string | no | Orchestration reference ID |
| `owner_tenant_id` | string | no | Owning tenant ID |
| `replication_factor` | integer | no | Number of replicas |
| `lifecycle_rules` | array | no | Object expiry rules |

**Request**
```json
{
  "operation": "bucket_create",
  "params": {
    "name": "tenant-c-uploads",
    "bucket_id": "bkt_003",
    "owner_tenant_id": "ten_c",
    "replication_factor": 2,
    "lifecycle_rules": [
      { "prefix": "tmp/", "expire_days": 7 }
    ]
  }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "bucket": "tenant-c-uploads", "status": "created" }
}
```

---

### `bucket_delete`

Deletes a bucket. Fails if the bucket is non-empty unless `force_empty` is set.

| Param | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Bucket name to delete |
| `force_empty` | boolean | no | Delete all objects first (default: false) |

**Request — safe delete (fails if non-empty)**
```json
{
  "operation": "bucket_delete",
  "params": { "name": "tenant-c-uploads" }
}
```

**Request — force delete**
```json
{
  "operation": "bucket_delete",
  "params": { "name": "tenant-c-uploads", "force_empty": true }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "bucket": "tenant-c-uploads", "status": "deleted" }
}
```

---

### `reconcile`

Re-runs the reconcile pass against the last `full_sync` desired state without
sending a new payload. Useful after a failed `full_sync` or manual intervention.

| Param | Type | Required | Values |
|---|---|---|---|
| `scope` | string | no | `"buckets"`, `"iam"`, or `"all"` (default) |

**Request**
```json
{
  "operation": "reconcile",
  "params": { "scope": "buckets" }
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "buckets_created": 0,
    "buckets_deleted": 1,
    "iam_created": 0,
    "iam_deleted": 0
  }
}
```

---

## 7. IAM Management

IAM operations read and write `/etc/seaweedfs/s3.json` directly and trigger
`systemctl reload weed-s3` after each change. Secret keys are never returned in responses.

### `s3.iam.list`

Lists all IAM identities currently in `s3.json`.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": [
    { "name": "tenant-a", "access_key": "AKIAIOSFODNN7TENANTA" },
    { "name": "tenant-b", "access_key": "AKIAIOSFODNN7TENANTB" }
  ]
}
```

---

### `iam_create`

Creates an IAM identity in `s3.json` and reloads the S3 gateway.

| Param | Type | Required | Description |
|---|---|---|---|
| `name` | string | yes | Identity name |
| `access_key` | string | yes | AWS-style access key (e.g. `AKIA_` prefix) |
| `secret_key` | string | yes | Secret key — min 64 chars recommended |
| `bucket_acls` | array | no | Per-bucket ACL entries (omit for full access) |
| `bucket_acls[].bucket_id` | string | yes | Bucket name prefix for ACL scope |
| `bucket_acls[].permission` | string | yes | `"r"`, `"rw"`, or `"admin"` |

**Request — full access (no ACL restrictions)**
```json
{
  "operation": "iam_create",
  "params": {
    "name": "tenant-c",
    "access_key": "AKIAIOSFODNN7TENANTC",
    "secret_key": "cSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyzABCDE"
  }
}
```

**Request — scoped bucket ACL**
```json
{
  "operation": "iam_create",
  "params": {
    "name": "tenant-c",
    "access_key": "AKIAIOSFODNN7TENANTC",
    "secret_key": "cSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyzABCDE",
    "bucket_acls": [
      { "bucket_id": "tenant-c-backups", "permission": "rw" },
      { "bucket_id": "tenant-c-logs",    "permission": "r"  }
    ]
  }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "name": "tenant-c", "status": "created" }
}
```

---

### `iam_delete`

Removes an IAM identity from `s3.json` and reloads the S3 gateway.

| Param | Type | Required |
|---|---|---|
| `name` | string | yes |

**Request**
```json
{
  "operation": "iam_delete",
  "params": { "name": "tenant-c" }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "name": "tenant-c", "status": "deleted" }
}
```

---

## 8. Nginx Customer Blocking

Blocking adds an `if ($http_authorization ~* "KEY") { return 403; }` rule to
`/etc/nginx/conf.d/s3_blocked_keys.conf` and reloads Nginx. The IAM identity
in `s3.json` is not touched — the block happens at the proxy layer only.

### `s3.blocked.list`

Lists all access keys currently blocked in `s3_blocked_keys.conf`.

**Params:** none

**Response**
```json
{
  "status": "completed",
  "output": [
    "AKIAIOSFODNN7TENANTB"
  ]
}
```

---

### `s3.customer.block`

Blocks an access key at the Nginx level and reloads Nginx.

| Param | Type | Required | Description |
|---|---|---|---|
| `access_key` | string | yes | The access key to block |
| `reason` | string | no | Written as a comment in the conf file (default: empty) |

**Request**
```json
{
  "operation": "s3.customer.block",
  "params": {
    "access_key": "AKIAIOSFODNN7TENANTB",
    "reason": "quota exceeded"
  }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "access_key": "AKIAIOSFODNN7TENANTB", "status": "blocked" }
}
```

---

### `s3.customer.unblock`

Removes an access key block and reloads Nginx.

| Param | Type | Required |
|---|---|---|
| `access_key` | string | yes |

**Request**
```json
{
  "operation": "s3.customer.unblock",
  "params": { "access_key": "AKIAIOSFODNN7TENANTB" }
}
```

**Response**
```json
{
  "status": "completed",
  "output": { "access_key": "AKIAIOSFODNN7TENANTB", "status": "unblocked" }
}
```

---

## 9. Exec

Runs an allowed binary directly (no shell). The binary must be listed in
`allowed_commands` in `agent.yaml` — unlisted paths are rejected without execution.

### `exec`

| Param | Type | Required | Description |
|---|---|---|---|
| `command` | string | yes | Absolute path to the binary |
| `args` | array | no | Arguments to pass |

**Request — journalctl**
```json
{
  "operation": "exec",
  "params": {
    "command": "/usr/bin/journalctl",
    "args": ["-u", "weed-s3", "-n", "50", "--no-pager"]
  }
}
```

**Request — df**
```json
{
  "operation": "exec",
  "params": {
    "command": "/usr/bin/df",
    "args": ["-h", "/data/seaweedfs"]
  }
}
```

**Request — ls**
```json
{
  "operation": "exec",
  "params": {
    "command": "/usr/bin/ls",
    "args": ["-lh", "/data/seaweedfs/volumes"]
  }
}
```

**Response**
```json
{
  "status": "completed",
  "output": {
    "stdout": "Filesystem      Size  Used Avail Use% Mounted on\n/dev/sda1        10T  2.0T  8.0T  20% /data/seaweedfs\n",
    "stderr": ""
  }
}
```

---

## 10. VM Power

> **Warning:** These operations are disabled by default in `agent.yaml`.
> Uncomment `vm.reboot` / `vm.shutdown` in `allowed_operations` to enable them.

### `vm.reboot`

Reboots the storage server immediately (`systemctl reboot`).

**Params:** none

**Request**
```json
{
  "operation": "vm.reboot"
}
```

---

### `vm.shutdown`

Shuts down the storage server immediately (`systemctl poweroff`).

**Params:** none

**Request**
```json
{
  "operation": "vm.shutdown"
}
```

---

## Error Responses

### `rejected`

The operation is not in `allowed_operations` or params could not be decoded.

```json
{
  "status": "rejected",
  "message": "operation \"vm.reboot\" is not permitted on this agent"
}
```

### `failed`

The operation was permitted and attempted but encountered a runtime error.

```json
{
  "status": "failed",
  "message": "creating bucket tenant-c-uploads: HTTP 409"
}
```

---

## Passive Events (Agent → Platform)

The agent also pushes these events without being asked:

| Type | Subject | Interval | Trigger |
|---|---|---|---|
| `heartbeat` | `agent.storage.{uuid}.evt` | 30 s | Scheduled |
| `telemetry` | `agent.storage.{uuid}.evt` | 30 s (configurable) | Scheduled |
| `s3_telemetry` | `agent.storage.{uuid}.evt` | 30 s | Scheduled |
| `s3_health` | `agent.storage.{uuid}.evt` | Immediate | State change |
| `capabilities` | `agent.storage.{uuid}.evt` | On boot + on `agent.allowed_operations` | Event |

`s3_health` is published immediately when any of the following cross a threshold:
- A service or API flips between reachable ↔ unreachable
- `degraded_volumes` changes from/to 0
- `capacity_pct` crosses 80 % (`capacity_warn_pct`) or 90 % (`capacity_critical_pct`)
