# s3d — Testing Guide

Quick-reference for sending commands to a running s3d agent via the NATS CLI.
For full operation parameters and response shapes see [operations.md](operations.md).

---

## Connection details

| Field | Value |
|---|---|
| WebSocket URL | `wss://nats.plusclouds.com:443` |
| Agent UUID | `d6199047-322a-4845-bdea-1d44dd1b49e5` |
| API key | `7Qic72PeIToUWqdiIEcfPxwgVBuNNcLRBjGxuoGEJefeu92NTqanK72JlBeeXBJ3` |
| Cmd subject | `agent.storage.d6199047-322a-4845-bdea-1d44dd1b49e5.cmd` |
| Evt subject | `agent.storage.d6199047-322a-4845-bdea-1d44dd1b49e5.evt` |

### NATS CLI shorthand

Set these once to avoid repeating them on every command:

```bash
export NATS_URL="wss://nats.plusclouds.com:443"
export NATS_USER="d6199047-322a-4845-bdea-1d44dd1b49e5"
export NATS_PASSWORD="7Qic72PeIToUWqdiIEcfPxwgVBuNNcLRBjGxuoGEJefeu92NTqanK72JlBeeXBJ3"
export CMD="agent.storage.d6199047-322a-4845-bdea-1d44dd1b49e5.cmd"
export EVT="agent.storage.d6199047-322a-4845-bdea-1d44dd1b49e5.evt"
```

Subscribe to responses in a separate terminal before sending commands:

```bash
nats sub "$EVT"
```

---

## Envelope format

Every message in both directions wraps in this envelope:

```json
{
  "v": 1,
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "type": "command",
  "agent_type": "storage",
  "agent_uuid": "d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp": 1749470000,
  "reply_to": "_INBOX.abc123",
  "payload": {
    "operation": "<operation-name>",
    "params": {},
    "timeout_s": 30
  }
}
```

- `reply_to` — set by the platform for synchronous request/reply; omit for fire-and-forget.
- `timestamp` — Unix epoch seconds; use `date +%s` on Linux.
- `id` — any unique string; use `uuidgen` or a fixed test value.

The agent result envelope looks like:

```json
{
  "v": 1,
  "id": "<new-uuid>",
  "type": "result",
  "agent_type": "storage",
  "agent_uuid": "d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp": 1749470001,
  "payload": {
    "command_id": "<id-from-command>",
    "status": "completed",
    "message": "",
    "output": {}
  }
}
```

Status values: `completed` | `failed` | `rejected`

---

## Sample commands

All examples use `nats pub "$CMD" '<json>'`.

---

### Cluster health

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-001","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"s3.cluster.status","params":{}}
}'
```

---

### Bucket stats

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-002","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"s3.bucket.stats","params":{}}
}'
```

---

### Full sync (desired state)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-003","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"full_sync",
    "params":{
      "buckets":[
        {
          "bucket_id":"bkt_001",
          "name":"tenant-a-backups",
          "owner_tenant_id":"ten_a",
          "replication_factor":2,
          "lifecycle_rules":[{"prefix":"logs/","expire_days":30}]
        },
        {
          "bucket_id":"bkt_002",
          "name":"tenant-b-archives",
          "owner_tenant_id":"ten_b",
          "replication_factor":1
        }
      ],
      "iam_users":[
        {
          "user_id":"iam_001",
          "name":"tenant-a",
          "owner_tenant_id":"ten_a",
          "access_key":"AKIAIOSFODNN7TENANTA",
          "secret_key":"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY_TENANT_A_64CHARS_LONG",
          "bucket_acls":[{"bucket_id":"bkt_001","permission":"rw"}]
        },
        {
          "user_id":"iam_002",
          "name":"tenant-b",
          "owner_tenant_id":"ten_b",
          "access_key":"AKIAIOSFODNN7TENANTB",
          "secret_key":"anotherSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwx1B",
          "bucket_acls":[{"bucket_id":"bkt_002","permission":"admin"}]
        }
      ]
    }
  }
}'
```

---

### Create bucket

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-004","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"bucket_create",
    "params":{
      "name":"tenant-c-uploads",
      "bucket_id":"bkt_003",
      "owner_tenant_id":"ten_c",
      "replication_factor":2,
      "lifecycle_rules":[{"prefix":"tmp/","expire_days":7}]
    }
  }
}'
```

---

### Delete bucket (safe)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-005","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"bucket_delete","params":{"name":"tenant-c-uploads"}}
}'
```

### Delete bucket (force empty)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-006","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"bucket_delete","params":{"name":"tenant-c-uploads","force_empty":true}}
}'
```

---

### IAM list

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-007","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"s3.iam.list","params":{}}
}'
```

---

### IAM create (full access)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-008","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"iam_create",
    "params":{
      "name":"tenant-c",
      "access_key":"AKIAIOSFODNN7TENANTC",
      "secret_key":"cSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyzABCDE"
    }
  }
}'
```

### IAM create (scoped ACL)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-009","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"iam_create",
    "params":{
      "name":"tenant-c",
      "access_key":"AKIAIOSFODNN7TENANTC",
      "secret_key":"cSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyzABCDE",
      "bucket_acls":[
        {"bucket_id":"tenant-c-uploads","permission":"rw"},
        {"bucket_id":"tenant-c-logs","permission":"r"}
      ]
    }
  }
}'
```

---

### IAM delete

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-010","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"iam_delete","params":{"name":"tenant-c"}}
}'
```

---

### Block access key

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-011","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"s3.customer.block",
    "params":{
      "access_key":"AKIAIOSFODNN7TENANTB",
      "reason":"non-payment suspension"
    }
  }
}'
```

---

### Unblock access key

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-012","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"s3.customer.unblock",
    "params":{"access_key":"AKIAIOSFODNN7TENANTB"}
  }
}'
```

---

### List blocked keys

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-013","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"s3.blocked.list","params":{}}
}'
```

---

### Reconcile (re-apply last full_sync)

```bash
nats pub "$CMD" '{
  "v":1,"id":"test-014","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"reconcile","params":{"scope":"all"}}
}'
```

---

### Service operations

```bash
# List all services
nats pub "$CMD" '{"v":1,"id":"test-015","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"services.list","params":{}}}'

# Get weed-s3 status
nats pub "$CMD" '{"v":1,"id":"test-016","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"services.get","params":{"name":"weed-s3"}}}'

# Reload weed-s3 (after manual IAM edit)
nats pub "$CMD" '{"v":1,"id":"test-017","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"services.reload","params":{"name":"weed-s3"}}}'

# Restart nginx
nats pub "$CMD" '{"v":1,"id":"test-018","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"services.restart","params":{"name":"nginx"}}}'
```

---

### Exec (allowlisted binaries only)

```bash
# Journal logs for weed-s3 (last 50 lines)
nats pub "$CMD" '{
  "v":1,"id":"test-019","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{
    "operation":"exec",
    "params":{
      "command":"/usr/bin/journalctl",
      "args":["-u","weed-s3","-n","50","--no-pager"]
    }
  }
}'

# Disk usage on SeaweedFS data volume
nats pub "$CMD" '{
  "v":1,"id":"test-020","type":"command",
  "agent_type":"storage",
  "agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5",
  "timestamp":1749470000,
  "payload":{"operation":"exec","params":{"command":"/usr/bin/df","args":["-h","/data/seaweedfs"]}}
}'
```

---

## s3dctl (local CLI — no NATS needed)

Run directly on the storage server:

```bash
# Preflight: all services + APIs + file permissions
s3dctl check

# Full cluster status table
s3dctl cluster status

# Detailed volume stats
s3dctl cluster volumes

# List buckets with sizes
s3dctl buckets list

# IAM management
s3dctl iam list
s3dctl iam create --name tenant-c --access-key AKIAIOSFODNN7TENANTC \
  --secret-key cSecretKey64charsLong1234567890abcdefghijklmnopqrstuvwxyzABCDE \
  --actions "Read:tenant-c-*,Write:tenant-c-*"
s3dctl iam delete tenant-c

# Nginx blocking
s3dctl blocked list
s3dctl blocked add AKIAIOSFODNN7TENANTB --reason "non-payment"
s3dctl blocked remove AKIAIOSFODNN7TENANTB
```

---

## End-to-end test sequence

```bash
# 1. Verify agent is alive
nats pub "$CMD" '{"v":1,"id":"e2e-01","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"s3.cluster.status","params":{}}}'

# 2. Create a test bucket
nats pub "$CMD" '{"v":1,"id":"e2e-02","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"bucket_create","params":{"name":"test-e2e","owner_tenant_id":"test"}}}'

# 3. Create a test IAM user scoped to that bucket
nats pub "$CMD" '{"v":1,"id":"e2e-03","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"iam_create","params":{"name":"test-user","access_key":"AKIATEST0000000000E2E","secret_key":"testSecretKey64charsLong0000000000000000000000000000000000000001","bucket_acls":[{"bucket_id":"test-e2e","permission":"rw"}]}}}'

# 4. Block the test user
nats pub "$CMD" '{"v":1,"id":"e2e-04","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"s3.customer.block","params":{"access_key":"AKIATEST0000000000E2E","reason":"e2e test block"}}}'

# 5. Verify block is listed
nats pub "$CMD" '{"v":1,"id":"e2e-05","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"s3.blocked.list","params":{}}}'

# 6. Teardown — unblock, delete IAM, delete bucket
nats pub "$CMD" '{"v":1,"id":"e2e-06","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"s3.customer.unblock","params":{"access_key":"AKIATEST0000000000E2E"}}}'
nats pub "$CMD" '{"v":1,"id":"e2e-07","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"iam_delete","params":{"name":"test-user"}}}'
nats pub "$CMD" '{"v":1,"id":"e2e-08","type":"command","agent_type":"storage","agent_uuid":"d6199047-322a-4845-bdea-1d44dd1b49e5","timestamp":1749470000,"payload":{"operation":"bucket_delete","params":{"name":"test-e2e","force_empty":true}}}'
```
