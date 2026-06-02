# SeaweedFS S3 Service — Architecture & Standardization Document

**Version:** 1.0  
**Status:** Draft  
**Last Updated:** 2026-05-26

---

## Table of Contents

1. [Overview](#1-overview)
2. [Infrastructure Architecture](#2-infrastructure-architecture)
3. [Software Stack](#3-software-stack)
4. [Storage Layout](#4-storage-layout)
5. [SeaweedFS Configuration](#5-seaweedfs-configuration)
6. [Nginx Configuration](#6-nginx-configuration)
7. [IAM & Customer Isolation](#7-iam--customer-isolation)
8. [Quota Management](#8-quota-management)
9. [Database Schema](#9-database-schema)
10. [Cron Jobs & Automation](#10-cron-jobs--automation)
11. [Customer Lifecycle](#11-customer-lifecycle)
12. [Monitoring & Alerting](#12-monitoring--alerting)
13. [Scaling Procedure](#13-scaling-procedure)
14. [Security Hardening](#14-security-hardening)
15. [Backup & Recovery](#15-backup--recovery)

---

## 1. Overview

This document defines the standard architecture, configuration, and operational procedures for the self-hosted S3-compatible object storage service built on **SeaweedFS**, served through **Nginx** with **TLS termination**, and managed with a **soft quota enforcement** system.

### Goals

- Provide a scalable, S3-compatible object storage service to multiple tenants
- Enforce per-customer storage and bandwidth quotas via soft enforcement
- Keep the system operationally simple while allowing horizontal scaling
- Maintain full data isolation between customers at the IAM level

### Non-Goals

- Real-time hard quota enforcement (planned for Phase 3)
- Per-request billing (planned for Phase 2)
- Multi-region replication (out of scope for Phase 1)

### Licensing

SeaweedFS is licensed under **Apache 2.0**. There are no commercial restrictions. All libraries and tooling built on top of SeaweedFS will be open-sourced under the same license.

---

## 2. Infrastructure Architecture

### Physical Layout

```
┌──────────────────────────────────────────────────────────────┐
│                    Barebone Server 1                         │
│                    Ubuntu 24.04 LTS                          │
│                                                              │
│  ┌─────────────────────┐   ┌────────────────────────────┐   │
│  │     NFS Server      │   │        SeaweedFS           │   │
│  │                     │   │                            │   │
│  │  /export/vms ───────┼──►│  master   :9333 (internal) │   │
│  │  (VM disks for      │   │  volume   :8080 (internal) │   │
│  │   hypervisors)      │   │  filer    :8888 (internal) │   │
│  │                     │   │  s3       :8333 (internal) │   │
│  └─────────────────────┘   └────────────────────────────┘   │
│                                         │                    │
│                             ┌───────────▼──────────────┐    │
│                             │          Nginx            │    │
│                             │   TLS :443  (public)      │    │
│                             │   HTTP :80  (redirect)    │    │
│                             └───────────────────────────┘    │
│                                                              │
│  RAID Controller                                             │
│  ├── /export/vms        (NFS — VM disk storage)              │
│  └── /data/seaweedfs    (SeaweedFS — S3 object storage)      │
└──────────────────────────────────────────────────────────────┘
                    │ (future)
┌──────────────────────────────────────────────────────────────┐
│                    Barebone Server 2                         │
│                    Ubuntu 24.04 LTS                          │
│                                                              │
│  ┌────────────────────────────┐                              │
│  │        SeaweedFS           │                              │
│  │  master   :9333 (peer)     │                              │
│  │  volume   :8080            │                              │
│  │  filer    :8888            │                              │
│  │  s3       :8333            │                              │
│  └────────────────────────────┘                              │
│                                                              │
│  RAID Controller                                             │
│  ├── /export/vms                                             │
│  └── /data/seaweedfs                                         │
└──────────────────────────────────────────────────────────────┘
```

### Network Zones

| Zone | Interface | Traffic | Exposed |
|---|---|---|---|
| Public | eth0 (or bond0) | S3 client requests via Nginx | Yes — port 443, 80 only |
| Internal | eth1 (optional) | NFS to hypervisors | No — internal network only |
| Storage | (same host) | SeaweedFS inter-component | No — localhost only |

> **Standard:** All SeaweedFS ports (8080, 8333, 8888, 9333) must be firewalled from public access at all times. Only Nginx on port 443 is publicly exposed.

---

## 3. Software Stack

| Component | Software | Version Policy |
|---|---|---|
| Operating System | Ubuntu 24.04 LTS | LTS only, patched monthly |
| Object Storage | SeaweedFS | Latest stable release |
| Reverse Proxy | Nginx | Ubuntu repo stable |
| TLS Certificates | Let's Encrypt (Certbot) | Auto-renewed |
| Quota Database | PostgreSQL 16 | Latest stable |
| Quota Enforcement | Custom Python 3 scripts | Internal repo |
| Log Processing | Custom Python 3 scripts | Internal repo |
| Process Management | systemd | OS default |
| Monitoring | Prometheus + Grafana | Latest stable |

---

## 4. Storage Layout

### Directory Structure

```
/
├── data/
│   └── seaweedfs/
│       ├── master/          # Master metadata (small, ~1GB max)
│       ├── filer/           # Filer metadata (leveldb)
│       └── volumes/         # Actual object data (bulk of storage)
│
├── export/
│   └── vms/                 # NFS exports for hypervisors
│
├── etc/
│   └── seaweedfs/
│       ├── s3.json          # IAM configuration (managed by scripts)
│       └── filer.toml       # Filer configuration
│
└── var/
    ├── log/
    │   ├── nginx/
    │   │   └── s3_access.log    # Parsed for bandwidth metering
    │   └── seaweedfs/
    └── lib/
        └── quota/               # Quota DB and scripts
```

### Disk Allocation Standard

When provisioning a new server, storage must be allocated as follows:

| Partition | Purpose | Recommended Size |
|---|---|---|
| `/` (OS) | Operating system | 50 GB |
| `/export/vms` | NFS VM disk storage | As agreed with hypervisor team |
| `/data/seaweedfs` | S3 object storage | Remainder of RAID volume |

> **Standard:** SeaweedFS and NFS data must never share the same filesystem mount point. Separation must be enforced at the directory level minimum, and at the partition level where possible.

---

## 5. SeaweedFS Configuration

### Replication Policy

| Servers Available | Replication Flag | Meaning |
|---|---|---|
| 1 server | `000` | No replication (single copy) |
| 2 servers | `001` | 2 copies across 2 different nodes |
| 3+ servers | `010` | 2 copies across 2 different racks |

> **Standard:** Always set `-defaultReplication=001` from day one even on a single server. This ensures that when Server 2 is added, replication activates automatically.

### Systemd Service Definitions

All SeaweedFS components are managed as systemd services. Startup order is:

```
weed-master → weed-volume → weed-filer → weed-s3
```

#### `/etc/systemd/system/weed-master.service`

```ini
[Unit]
Description=SeaweedFS Master
After=network.target
Wants=network.target

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=/usr/local/bin/weed master \
  -ip=${SERVER_IP} \
  -port=9333 \
  -mdir=/data/seaweedfs/master \
  -defaultReplication=001 \
  -volumeSizeLimitMB=30000
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

#### `/etc/systemd/system/weed-volume.service`

```ini
[Unit]
Description=SeaweedFS Volume
After=weed-master.service
Requires=weed-master.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=/usr/local/bin/weed volume \
  -ip=${SERVER_IP} \
  -port=8080 \
  -mserver=${SERVER_IP}:9333 \
  -dir=/data/seaweedfs/volumes \
  -max=0 \
  -minFreeSpacePercent=5
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

> `-minFreeSpacePercent=5` stops accepting writes when disk is 95% full, preventing the server from completely filling up.

#### `/etc/systemd/system/weed-filer.service`

```ini
[Unit]
Description=SeaweedFS Filer
After=weed-master.service
Requires=weed-master.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=/usr/local/bin/weed filer \
  -ip=${SERVER_IP} \
  -port=8888 \
  -master=${SERVER_IP}:9333
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

#### `/etc/systemd/system/weed-s3.service`

```ini
[Unit]
Description=SeaweedFS S3 Gateway
After=weed-filer.service
Requires=weed-filer.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=/usr/local/bin/weed s3 \
  -ip=127.0.0.1 \
  -port=8333 \
  -filer=${SERVER_IP}:8888 \
  -config=/etc/seaweedfs/s3.json
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

> **Standard:** The S3 gateway must always bind to `127.0.0.1` only, never `0.0.0.0`. Nginx is the only allowed entrypoint.

---

## 6. Nginx Configuration

### TLS Standard

- TLS 1.2 minimum, TLS 1.3 preferred
- Certificates via Let's Encrypt (Certbot), auto-renewed
- HTTP (port 80) always redirects to HTTPS (port 443)
- HSTS header enabled

### S3 Endpoint Nginx Config

**File:** `/etc/nginx/sites-available/s3`

```nginx
# Redirect HTTP to HTTPS
server {
    listen 80;
    server_name s3.yourdomain.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name s3.yourdomain.com;

    # TLS
    ssl_certificate     /etc/letsencrypt/live/s3.yourdomain.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/s3.yourdomain.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;
    ssl_session_cache   shared:SSL:10m;
    add_header Strict-Transport-Security "max-age=31536000" always;

    # Quota: block suspended customers (managed by quota script)
    # Keys are added to this file by the enforcement cron job
    include /etc/nginx/conf.d/s3_blocked_keys.conf;

    # Rate limiting per access key
    limit_req_zone $http_authorization zone=s3_per_key:10m rate=100r/s;
    limit_req zone=s3_per_key burst=200 nodelay;

    # Access logging for bandwidth metering
    log_format s3_quota '$time_iso8601 $http_authorization '
                        '$request_method $request_uri '
                        '$status $bytes_sent $request_length';
    access_log /var/log/nginx/s3_access.log s3_quota;

    # Upload size limit (adjust per your largest expected object)
    client_max_body_size 50G;
    client_body_timeout 300s;

    # Proxy to SeaweedFS S3 gateway
    location / {
        proxy_pass         http://127.0.0.1:8333;
        proxy_set_header   Host $host;
        proxy_set_header   X-Real-IP $remote_addr;
        proxy_set_header   X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;

        # Required for large uploads
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      300s;
        proxy_send_timeout      300s;
    }
}
```

### Blocked Keys File Format

**File:** `/etc/nginx/conf.d/s3_blocked_keys.conf`  
Managed automatically by the quota enforcement script.

```nginx
# AUTO-GENERATED — do not edit manually
# Last updated: 2026-05-26 14:00:00

# customer-b (quota exceeded — blocked 2026-05-25)
if ($http_authorization ~* "AKIAIOSFODNN7CUSTOMERB") {
    return 403 "Storage quota exceeded. Please contact support.";
}
```

> **Standard:** This file must never be edited manually. It is owned and written exclusively by the quota enforcement script.

---

## 7. IAM & Customer Isolation

### IAM Configuration File

**File:** `/etc/seaweedfs/s3.json`  
Managed automatically by the customer provisioning script.

```json
{
  "identities": [
    {
      "name": "customer-a",
      "credentials": [
        {
          "accessKey": "AKIA_CUSTOMER_A_KEY",
          "secretKey": "secret_key_customer_a_64chars"
        }
      ],
      "actions": [
        "Read:customer-a-*",
        "Write:customer-a-*",
        "List:customer-a-*",
        "Tagging:customer-a-*",
        "Admin:customer-a-*"
      ]
    },
    {
      "name": "customer-b",
      "credentials": [
        {
          "accessKey": "AKIA_CUSTOMER_B_KEY",
          "secretKey": "secret_key_customer_b_64chars"
        }
      ],
      "actions": [
        "Read:customer-b-*",
        "Write:customer-b-*",
        "List:customer-b-*",
        "Tagging:customer-b-*",
        "Admin:customer-b-*"
      ]
    }
  ]
}
```

### IAM Standards

| Rule | Standard |
|---|---|
| Access key format | `AKIA_` prefix + customer slug + random 16 chars (uppercase alphanumeric) |
| Secret key length | Minimum 64 characters, cryptographically random |
| Bucket prefix | Always matches customer slug (e.g. `customer-a-*`) |
| Key rotation | Supported — provisioning script handles rotation without downtime |
| Admin identity | One separate admin identity for internal operations only |

### Applying IAM Changes

After any change to `s3.json`, SeaweedFS must reload its config:

```bash
# Signal SeaweedFS S3 to reload config (no restart needed)
systemctl reload weed-s3

# Verify reload
journalctl -u weed-s3 -n 20
```

> **Standard:** Never restart `weed-s3` to apply IAM changes. Always use `systemctl reload`. A restart causes brief downtime for all customers.

---

## 8. Quota Management

### Enforcement Model: Soft Quota

Soft quota enforcement means:

- Customers **can** temporarily exceed their quota between check cycles
- Enforcement runs every **15 minutes** via cron
- When quota is exceeded, the customer's access key is **blocked at Nginx level** within 15 minutes
- SeaweedFS IAM key remains in `s3.json` but is blocked at the proxy layer
- Customers receive email notification at **80%** and **100%** usage

### Quota Dimensions

| Dimension | Enforced | Method |
|---|---|---|
| Storage (bytes) | ✅ Yes | SeaweedFS usage API → cron |
| Object count | ✅ Yes | SeaweedFS usage API → cron |
| Monthly egress (bytes) | ✅ Yes | Nginx log parser → cron |
| Request rate | ✅ Yes | Nginx `limit_req` (real-time) |
| Bucket count | ✅ Yes | Provisioning script (at creation time) |

### Enforcement Flow

```
Every 15 minutes:

┌─────────────────────────────────────────────────────┐
│                  quota-check.py                     │
│                                                     │
│  1. For each active customer in DB:                 │
│     a. Query SeaweedFS API for storage used         │
│     b. Query SeaweedFS API for object count         │
│     c. Read monthly egress from bandwidth table     │
│     d. Write snapshot to usage table                │
│                                                     │
│  2. Compare usage against quota limits              │
│                                                     │
│  3. If storage > 80% or egress > 80%:               │
│     → Send warning email (once per threshold)       │
│                                                     │
│  4. If storage > 100% or egress > 100%:             │
│     → Add customer key to s3_blocked_keys.conf      │
│     → nginx -s reload                               │
│     → Update customer status = 'blocked' in DB      │
│     → Send quota exceeded email                     │
│                                                     │
│  5. If customer is blocked AND usage < 90%:         │
│     → Do NOT auto-unblock (manual intervention)     │
│     → Only unblock via admin script                 │
└─────────────────────────────────────────────────────┘
```

> **Standard:** Customers are **never automatically unblocked**. Unblocking is always a manual admin action (upgrade plan, payment, or explicit override) to prevent abuse.

---

## 9. Database Schema

**Database:** PostgreSQL 16  
**Database name:** `s3quota`  
**User:** `quotauser` (no superuser privileges)

```sql
-- Customers table
CREATE TABLE customers (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    slug            TEXT UNIQUE NOT NULL,       -- e.g. 'customer-a'
    name            TEXT NOT NULL,
    email           TEXT NOT NULL,
    access_key      TEXT UNIQUE NOT NULL,
    secret_key      TEXT NOT NULL,              -- stored encrypted
    status          TEXT NOT NULL DEFAULT 'active',
                                                -- active | blocked | suspended | deleted
    created_at      TIMESTAMPTZ DEFAULT NOW(),
    updated_at      TIMESTAMPTZ DEFAULT NOW()
);

-- Quota limits per customer
CREATE TABLE quotas (
    customer_id         UUID PRIMARY KEY REFERENCES customers(id),
    storage_bytes       BIGINT NOT NULL,        -- max storage in bytes
    egress_bytes_month  BIGINT NOT NULL,        -- max egress per calendar month
    max_buckets         INTEGER NOT NULL DEFAULT 10,
    max_objects         BIGINT NOT NULL DEFAULT 10000000,
    updated_at          TIMESTAMPTZ DEFAULT NOW()
);

-- Usage snapshots (written every 15 min by cron)
CREATE TABLE usage_snapshots (
    id              BIGSERIAL PRIMARY KEY,
    customer_id     UUID REFERENCES customers(id),
    snapshot_at     TIMESTAMPTZ DEFAULT NOW(),
    storage_bytes   BIGINT NOT NULL,
    object_count    BIGINT NOT NULL
);

-- Index for fast recent lookup
CREATE INDEX idx_usage_customer_time
    ON usage_snapshots (customer_id, snapshot_at DESC);

-- Monthly bandwidth tracking (updated by log parser cron)
CREATE TABLE bandwidth_monthly (
    customer_id     UUID REFERENCES customers(id),
    month           DATE NOT NULL,              -- always first of month
    egress_bytes    BIGINT NOT NULL DEFAULT 0,
    ingress_bytes   BIGINT NOT NULL DEFAULT 0,
    PRIMARY KEY (customer_id, month)
);

-- Notification tracking (avoid duplicate emails)
CREATE TABLE notifications_sent (
    id              BIGSERIAL PRIMARY KEY,
    customer_id     UUID REFERENCES customers(id),
    notification    TEXT NOT NULL,              -- 'warning_80_storage', 'exceeded_storage', etc.
    sent_at         TIMESTAMPTZ DEFAULT NOW(),
    month           DATE NOT NULL
);

-- Audit log for all admin actions
CREATE TABLE admin_audit (
    id              BIGSERIAL PRIMARY KEY,
    action          TEXT NOT NULL,              -- 'block', 'unblock', 'provision', 'delete'
    customer_id     UUID REFERENCES customers(id),
    performed_by    TEXT NOT NULL,              -- admin username
    reason          TEXT,
    performed_at    TIMESTAMPTZ DEFAULT NOW()
);
```

---

## 10. Cron Jobs & Automation

### Cron Schedule

```
# /etc/cron.d/s3quota

# Quota check and enforcement — every 15 minutes
*/15 * * * * seaweedfs /usr/local/bin/quota-check.py >> /var/log/quota/check.log 2>&1

# Nginx log bandwidth parser — every hour
0 * * * * seaweedfs /usr/local/bin/bandwidth-parse.py >> /var/log/quota/bandwidth.log 2>&1

# Monthly bandwidth reset — 1st of each month at 00:05
5 0 1 * * seaweedfs /usr/local/bin/bandwidth-reset.py >> /var/log/quota/reset.log 2>&1

# Disk usage summary report — daily at 07:00
0 7 * * * seaweedfs /usr/local/bin/daily-report.py >> /var/log/quota/report.log 2>&1
```

### Script Responsibilities

#### `quota-check.py`
- Queries SeaweedFS API: `GET http://localhost:8888/buckets` for each customer bucket
- Writes usage snapshot to `usage_snapshots` table
- Compares against `quotas` table
- Sends warning emails at 80% (once per month per dimension)
- Blocks customer at Nginx level if quota exceeded
- Logs all actions to `/var/log/quota/check.log`

#### `bandwidth-parse.py`
- Reads new lines from `/var/log/nginx/s3_access.log` since last run (uses bookmark file)
- Parses `$http_authorization` to identify customer by access key
- Aggregates `$bytes_sent` per customer
- Upserts into `bandwidth_monthly` table
- Stores bookmark in `/var/lib/quota/nginx_log_bookmark`

#### `bandwidth-reset.py`
- Runs on the 1st of each month
- Inserts new rows in `bandwidth_monthly` for the new month (starting at 0)
- Unblocks customers who were blocked only for egress (not storage) violations
- Logs reset actions to audit log

#### `daily-report.py`
- Generates a summary of all customer usage vs quota
- Emails report to admin addresses defined in config
- Flags customers above 70% on any dimension

### Script Configuration File

**File:** `/etc/s3quota/config.toml`

```toml
[database]
host     = "localhost"
port     = 5432
name     = "s3quota"
user     = "quotauser"
password = "changeme"

[seaweedfs]
filer_url  = "http://localhost:8888"
master_url = "http://localhost:9333"

[nginx]
blocked_keys_file = "/etc/nginx/conf.d/s3_blocked_keys.conf"
access_log        = "/var/log/nginx/s3_access.log"
log_bookmark      = "/var/lib/quota/nginx_log_bookmark"

[email]
smtp_host   = "smtp.yourdomain.com"
smtp_port   = 587
smtp_user   = "noreply@yourdomain.com"
smtp_pass   = "changeme"
from_addr   = "S3 Storage <noreply@yourdomain.com>"
admin_email = ["admin@yourdomain.com"]

[thresholds]
warning_percent  = 80       # send warning email at this % of quota
block_percent    = 100      # block at this % of quota
min_free_gb      = 50       # alert admin if total free space below this
```

---

## 11. Customer Lifecycle

### Provisioning a New Customer

Run the provisioning script:

```bash
/usr/local/bin/provision-customer.py \
  --slug "customer-a" \
  --name "Acme Corp" \
  --email "admin@acme.com" \
  --storage-gb 100 \
  --egress-gb 500 \
  --max-buckets 10
```

The script performs the following steps in order:

1. Generate cryptographically random access key and secret key
2. Insert customer record into `customers` table
3. Insert quota record into `quotas` table
4. Append IAM identity to `/etc/seaweedfs/s3.json`
5. Reload SeaweedFS S3 gateway (`systemctl reload weed-s3`)
6. Create default bucket `{slug}-default` via S3 API
7. Send welcome email to customer with credentials
8. Write action to `admin_audit` table

### Blocking a Customer (Manual)

```bash
/usr/local/bin/admin-customer.py block \
  --slug "customer-a" \
  --reason "Payment overdue" \
  --admin "yourname"
```

### Unblocking a Customer (Manual)

```bash
/usr/local/bin/admin-customer.py unblock \
  --slug "customer-a" \
  --reason "Payment received" \
  --admin "yourname"
```

### Deleting a Customer

```bash
# Soft delete — blocks access, retains data for 30 days
/usr/local/bin/admin-customer.py delete \
  --slug "customer-a" \
  --admin "yourname"

# Hard delete — permanently removes data (irreversible)
/usr/local/bin/admin-customer.py delete \
  --slug "customer-a" \
  --purge \
  --admin "yourname"
```

> **Standard:** Hard delete requires two-person confirmation. The second admin must confirm via a separate terminal session before data is removed.

---

## 12. Monitoring & Alerting

### Metrics to Collect

| Metric | Source | Alert Threshold |
|---|---|---|
| Total disk free | Node exporter | < 10% free |
| SeaweedFS volume free | SeaweedFS API | < 5% free |
| S3 request rate | Nginx log | Sudden 10x spike |
| S3 error rate (5xx) | Nginx log | > 1% of requests |
| Customer quota usage | quota DB | > 70% (report), > 90% (alert) |
| SeaweedFS master health | Master API | Any downtime |
| Cron job failures | Syslog | Any failure |

### Health Check Endpoints

```bash
# SeaweedFS master health
curl http://localhost:9333/cluster/status

# SeaweedFS volume health
curl http://localhost:8080/status

# SeaweedFS filer health
curl http://localhost:8888/

# S3 gateway (via Nginx)
curl https://s3.yourdomain.com/healthz
```

---

## 13. Scaling Procedure

When adding a second (or subsequent) server:

### Pre-requisites

- New server provisioned with Ubuntu 24.04 LTS
- Same software stack installed
- Network connectivity between Server 1 and Server 2 confirmed
- `/data/seaweedfs` mounted on new server

### Steps

1. Install SeaweedFS binary (same version as Server 1)
2. Configure systemd services with Server 2's IP
3. Start `weed-master` on Server 2 pointing to Server 1 as peer:
   ```
   -peers=SERVER1_IP:9333
   ```
4. Start `weed-volume` on Server 2 pointing to both masters:
   ```
   -mserver=SERVER1_IP:9333,SERVER2_IP:9333
   ```
5. Start `weed-filer` on Server 2
6. Start `weed-s3` on Server 2
7. Update Nginx on Server 1 to load balance across both S3 gateways
8. Update monitoring to include Server 2

> **Standard:** Never add a server under load without first testing connectivity and verifying the master Raft cluster has formed correctly via `curl http://SERVER1_IP:9333/cluster/status`.

---

## 14. Security Hardening

### OS Level

```bash
# Dedicated service user — no login shell
useradd -r -s /usr/sbin/nologin -d /data/seaweedfs seaweedfs

# Firewall — only expose what is necessary
ufw default deny incoming
ufw allow 22/tcp    # SSH (restrict to admin IPs in production)
ufw allow 80/tcp    # HTTP (Nginx redirect only)
ufw allow 443/tcp   # HTTPS (Nginx — S3 endpoint)
ufw enable

# SeaweedFS ports are NOT opened — all internal only
```

### SeaweedFS Level

- S3 gateway binds to `127.0.0.1` only
- All components run as `seaweedfs` user (non-root)
- `s3.json` permissions: `640`, owned by `seaweedfs:seaweedfs`
- Secret keys in `s3.json` must be rotated every 90 days

### Nginx Level

- TLS 1.2+ only
- HSTS enabled
- Server tokens hidden (`server_tokens off`)
- Rate limiting per customer key
- Request size limit enforced

### Database Level

- PostgreSQL listens on `localhost` only
- `quotauser` has no DDL privileges (SELECT, INSERT, UPDATE, DELETE only)
- Database credentials stored in `/etc/s3quota/config.toml` with permissions `600`

---

## 15. Backup & Recovery

### What to Back Up

| Data | Location | Frequency | Method |
|---|---|---|---|
| SeaweedFS master metadata | `/data/seaweedfs/master` | Daily | rsync to offsite |
| SeaweedFS filer metadata | `/data/seaweedfs/filer` | Daily | rsync to offsite |
| IAM config | `/etc/seaweedfs/s3.json` | On every change | Git repository |
| Quota DB | PostgreSQL `s3quota` | Daily | `pg_dump` to offsite |
| Nginx config | `/etc/nginx/` | On every change | Git repository |
| Quota scripts | `/usr/local/bin/quota-*.py` | On every change | Git repository |

> **Standard:** Customer object data (in `/data/seaweedfs/volumes`) is protected by SeaweedFS replication across servers, not by traditional backup. Single-server deployments carry data loss risk and must be clearly documented in customer SLAs.

### Recovery Procedure (Master Failure)

1. Stop all SeaweedFS services
2. Restore `/data/seaweedfs/master` from backup
3. Start `weed-master` — volume servers re-register automatically
4. Start remaining services in order
5. Verify cluster status via API

---

*End of Document*

---

> **Document Owner:** Infrastructure Team  
> **Review Cycle:** Quarterly or after any major architecture change  
> **Repository:** `git@yourdomain.com:infra/s3-service-standard.git`
