# s3d — Installation Guide (Ubuntu 24.04 LTS)

This guide sets up a single-node SeaweedFS S3 cluster with Nginx TLS termination
and the `s3d` storage agent on a fresh Ubuntu 24.04 LTS server.

---

## Table of Contents

1. [Prerequisites](#1-prerequisites)
2. [OS baseline](#2-os-baseline)
3. [Create service user and directories](#3-create-service-user-and-directories)
4. [Firewall](#4-firewall)
5. [Install SeaweedFS](#5-install-seaweedfs)
6. [SeaweedFS configuration files](#6-seaweedfs-configuration-files)
7. [systemd service units](#7-systemd-service-units)
8. [Start and verify SeaweedFS](#8-start-and-verify-seaweedfs)
9. [Install Nginx](#9-install-nginx)
10. [TLS certificate (Let's Encrypt)](#10-tls-certificate-lets-encrypt)
11. [Nginx site configuration](#11-nginx-site-configuration)
12. [Install s3d agent](#12-install-s3d-agent)
13. [s3d configuration](#13-s3d-configuration)
14. [s3d systemd service](#14-s3d-systemd-service)
15. [Verify full stack](#15-verify-full-stack)
16. [WORM / Object Lock buckets](#16-worm--object-lock-buckets)
17. [Post-install checklist](#17-post-install-checklist)

---

## 1. Prerequisites

### Hardware (minimum)

| Resource | Minimum | Recommended |
|---|---|---|
| CPU | 4 cores | 8+ cores |
| RAM | 8 GB | 32 GB |
| OS disk | 50 GB SSD | 100 GB SSD |
| Data disk | 500 GB | 10 TB+ (RAID) |
| Network | 1 Gbps | 10 Gbps |

### Software

- Ubuntu 24.04 LTS (fresh install, no prior SeaweedFS)
- A public domain name pointing to this server (for TLS)
- Outbound internet access (package downloads, Let's Encrypt)

### Variables used in this guide

Replace these values with your own everywhere they appear:

```
SERVER_IP=185.255.172.184       # Public IP of this server
PUBLIC_DOMAIN=dc.s3.example.com # Public S3 endpoint hostname
```

---

## 2. OS baseline

```bash
apt-get update && apt-get upgrade -y

apt-get install -y \
  curl wget jq unzip \
  nginx certbot python3-certbot-nginx \
  ufw

# Optional but helps with logs
hostnamectl set-hostname storage01
```

---

## 3. Create service user and directories

SeaweedFS runs as a dedicated non-root user. The `s3d` agent runs as root
(required for systemd D-Bus access).

```bash
# Create the seaweedfs system user
useradd --system --no-create-home --shell /usr/sbin/nologin seaweedfs

# Data directories
mkdir -p /data/seaweedfs/master
mkdir -p /data/seaweedfs/volumes
mkdir -p /data/seaweedfs/filer
chown -R seaweedfs:seaweedfs /data/seaweedfs

# Config directory
mkdir -p /etc/seaweedfs
chown seaweedfs:seaweedfs /etc/seaweedfs

# Nginx blocked-keys include directory (written by s3d)
mkdir -p /etc/nginx/conf.d

# Log directory for the agent
mkdir -p /var/log/plusclouds
```

---

## 4. Firewall

SeaweedFS internal ports must never be exposed to the internet.
Only Nginx (80/443) and SSH are public-facing.

```bash
ufw --force enable

ufw allow 22/tcp  comment "SSH"
ufw allow 80/tcp  comment "HTTP → HTTPS redirect"
ufw allow 443/tcp comment "HTTPS S3 endpoint"

ufw status verbose
```

> **Important:** Never open ports 8080, 8333, 8888, or 9333 in the firewall.
> All client traffic must go through Nginx on port 443.

---

## 5. Install SeaweedFS

SeaweedFS ships as a single static binary called `weed`.

```bash
WEED_VERSION=$(curl -s https://api.github.com/repos/seaweedfs/seaweedfs/releases/latest \
  | jq -r '.tag_name' | tr -d 'v')

echo "Installing SeaweedFS $WEED_VERSION"

cd /tmp
wget "https://github.com/seaweedfs/seaweedfs/releases/download/${WEED_VERSION}/linux_amd64_large_disk.tar.gz" \
  -O weed.tar.gz

tar -xzf weed.tar.gz
mv weed /usr/local/bin/weed
chmod +x /usr/local/bin/weed

weed version
```

> The `large_disk` build supports volumes larger than 30 GB. Always use it.

---

## 6. SeaweedFS configuration files

### Filer metadata backend

```bash
cat > /etc/seaweedfs/filer.toml << 'EOF'
[leveldb2]
enabled = true
dir = "/data/seaweedfs/filer"
EOF

chown seaweedfs:seaweedfs /etc/seaweedfs/filer.toml
```

### Initial IAM file

The S3 gateway requires a valid `s3.json` on startup. Start with an empty
identities list — `s3d` will seed the admin identity on first start.

```bash
cat > /etc/seaweedfs/s3.json << 'EOF'
{
  "identities": []
}
EOF

chown seaweedfs:seaweedfs /etc/seaweedfs/s3.json
chmod 640 /etc/seaweedfs/s3.json
```

> **Ownership matters:** `weed-s3` runs as the `seaweedfs` user. If `s3.json` is
> owned by root, the gateway will crash with `permission denied` on every restart.
> Always keep it `seaweedfs:seaweedfs 640`.

### Nginx traffic log format

`s3d` tails the Nginx access log to track per-bucket upload and download bytes.
Create the log format definition — it must live in `conf.d/` so Nginx loads it
inside the `http` block before the site config references it.

```bash
cat > /etc/nginx/conf.d/s3_log_format.conf << 'EOF'
log_format s3_traffic "$msec $request_method $request_uri $status $bytes_sent $request_length";
EOF
```

> Fields: unix timestamp · method · URI · HTTP status · bytes sent to client · bytes received from client.

### Initial Nginx blocked-keys include

```bash
cat > /etc/nginx/conf.d/s3_blocked_keys.conf << 'EOF'
# Managed by s3d — do not edit manually
EOF
```

---

## 7. systemd service units

Copy each file exactly as shown. Replace `SERVER_IP` with your server's IP address.

### `/etc/systemd/system/weed-master.service`

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
  -ip=SERVER_IP \
  -port=9333 \
  -mdir=/data/seaweedfs/master \
  -defaultReplication=000 \
  -volumeSizeLimitMB=30000
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-master

[Install]
WantedBy=multi-user.target
```

> `-defaultReplication=000` is correct for a single-node setup (no replicas).
> Use `001` only if you have a second storage server.

### `/etc/systemd/system/weed-volume.service`

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
  -ip=SERVER_IP \
  -port=8080 \
  -mserver=SERVER_IP:9333 \
  -dir=/data/seaweedfs/volumes \
  -max=50 \
  -minFreeSpacePercent=5
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-volume

[Install]
WantedBy=multi-user.target
```

> **Critical — always set `-max` explicitly.**
> With `-max=0` (auto), SeaweedFS calculates the limit as `floor(disk_GB / 30)`.
> On an 80 GB OS disk that gives `-max=2`. Once both slots are occupied, the master
> reports "0 node candidates" and every authenticated PUT to a new bucket collection
> hangs indefinitely with no error. Use `-max=50` as a safe default; volume files
> stay small until they are actually filled.

### `/etc/systemd/system/weed-filer.service`

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
  -ip=SERVER_IP \
  -port=8888 \
  -master=SERVER_IP:9333 \
  -defaultStoreDir=/data/seaweedfs/filer
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-filer

[Install]
WantedBy=multi-user.target
```

### `/etc/systemd/system/weed-s3.service`

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
  -port=8333 \
  -filer=SERVER_IP:8888 \
  -config=/etc/seaweedfs/s3.json
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-s3

[Install]
WantedBy=multi-user.target
```

> The S3 gateway binds to all interfaces but is protected by the firewall (port 8333
> is not open). All external traffic goes through Nginx.
> **Note:** `weed-s3` has no `ExecReload=` handler. Use `systemctl restart weed-s3`
> after config changes — `systemctl reload weed-s3` exits with code 3.

### Reload systemd and enable all services

```bash
systemctl daemon-reload
systemctl enable weed-master weed-volume weed-filer weed-s3
```

---

## 8. Start and verify SeaweedFS

Start in dependency order and verify each step before proceeding.

```bash
# Start master
systemctl start weed-master
sleep 3
curl -s http://localhost:9333/cluster/status | jq .
# Expected: {"IsLeader":true,"Leader":"SERVER_IP:9333.19333","MaxVolumeId":0}

# Start volume
systemctl start weed-volume
sleep 3
curl -s http://localhost:9333/vol/status | jq '.Volumes | {Free, Max}'
# Expected: {"Free": 48, "Max": 50}

# Start filer
systemctl start weed-filer
sleep 5
curl -s -o /dev/null -w "%{http_code}" http://localhost:8888/
# Expected: 200

# Start S3 gateway
systemctl start weed-s3
sleep 3
curl -s -o /dev/null -w "%{http_code}" http://localhost:8333/
# Expected: 200

# Confirm all four active
systemctl is-active weed-master weed-volume weed-filer weed-s3
# Expected: active active active active
```

### Verify volume assignment works

Before moving on, confirm the master can allocate volumes (this will catch the
`-max` issue early):

```bash
curl -s "http://localhost:9333/dir/assign"
# Expected: {"fid":"1,xxxx","url":"SERVER_IP:8080",...}

curl -s "http://localhost:9333/dir/assign?collection=test-bucket"
# Expected: same shape — must return a fid, not hang
```

If the second command hangs, check `vol/status` — `Free` is probably 0.
Fix: update `-max` in `weed-volume.service`, reload, restart the unit.

Check logs if any service fails:

```bash
journalctl -u weed-master -n 50 --no-pager
journalctl -u weed-volume -n 50 --no-pager
journalctl -u weed-filer  -n 50 --no-pager
journalctl -u weed-s3     -n 50 --no-pager
```

---

## 9. Install Nginx

```bash
# Already installed in section 2 — skip if done
apt-get install -y nginx

rm -f /etc/nginx/sites-enabled/default

systemctl enable nginx
systemctl start nginx
```

---

## 10. TLS certificate (Let's Encrypt)

The DNS A record for your domain must already point to `SERVER_IP` before running
this. Certbot will fail the ACME HTTP-01 challenge if DNS has not propagated yet.

First create a minimal HTTP-only site so Certbot has something to validate against:

```bash
cat > /etc/nginx/sites-available/s3 << 'EOF'
server {
    listen 80;
    server_name PUBLIC_DOMAIN;
    root /var/www/html;
}
EOF

ln -s /etc/nginx/sites-available/s3 /etc/nginx/sites-enabled/s3
nginx -t && systemctl reload nginx
```

Then obtain the certificate:

```bash
certbot --nginx -d PUBLIC_DOMAIN \
  --non-interactive \
  --agree-tos \
  --email admin@example.com

# Test auto-renewal
certbot renew --dry-run
```

Certbot rewrites the nginx config automatically to add TLS and the HTTP→HTTPS
redirect. The final site config is described in the next section.

---

## 11. Nginx site configuration

After Certbot runs, replace `/etc/nginx/sites-available/s3` with the full
proxy config. Certbot will preserve the TLS blocks it added.

```bash
cat > /etc/nginx/sites-available/s3 << 'EOF'
server {
    server_name PUBLIC_DOMAIN;

    # Blocked access keys (managed by s3d at runtime)
    include /etc/nginx/conf.d/s3_blocked_keys.conf;

    # Allow large uploads
    client_max_body_size 50G;
    client_body_timeout  300s;

    access_log /var/log/nginx/s3_access.log s3_traffic;

    location / {
        proxy_pass         http://127.0.0.1:8333;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;

        # Required for streaming uploads — do not buffer
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      300s;
        proxy_send_timeout      300s;
    }

    listen 443 ssl; # managed by Certbot
    ssl_certificate /etc/letsencrypt/live/PUBLIC_DOMAIN/fullchain.pem; # managed by Certbot
    ssl_certificate_key /etc/letsencrypt/live/PUBLIC_DOMAIN/privkey.pem; # managed by Certbot
    include /etc/letsencrypt/options-ssl-nginx.conf; # managed by Certbot
    ssl_dhparam /etc/letsencrypt/ssl-dhparams.pem; # managed by Certbot
}

server {
    if ($host = PUBLIC_DOMAIN) {
        return 301 https://$host$request_uri;
    } # managed by Certbot

    listen 80;
    server_name PUBLIC_DOMAIN;
    return 404; # managed by Certbot
}
EOF

nginx -t && systemctl reload nginx
```

Verify TLS is working:

```bash
curl -s -o /dev/null -w "%{http_code}" https://PUBLIC_DOMAIN/
# Expected: 403 (unauthenticated — correct, gateway is responding)
```

---

## 12. Install s3d agent

### Option A — build from source (recommended for this repo)

The project requires Go 1.22+. The `go.mod` specifies the toolchain; the build
command below uses the Go toolchain cache to satisfy it automatically.

```bash
# Install Go (system package is fine as a bootstrap)
apt-get install -y golang-go

# Clone the repo
git clone https://github.com/plusclouds/s3.agent.git /opt/s3.agent
cd /opt/s3.agent

# Build for Linux amd64 (uses toolchain cache in GOPATH)
GOPATH=/root/gopath make build-linux
# Outputs: bin/s3d.linux, bin/s3dctl.linux

cp bin/s3d.linux    /usr/local/bin/s3d
cp bin/s3dctl.linux /usr/local/bin/s3dctl
chmod +x /usr/local/bin/s3d /usr/local/bin/s3dctl
```

### Option B — pre-built binary

```bash
curl -L https://releases.internal.plusclouds.com/s3d/latest/s3d.linux \
  -o /usr/local/bin/s3d
curl -L https://releases.internal.plusclouds.com/s3d/latest/s3dctl.linux \
  -o /usr/local/bin/s3dctl
chmod +x /usr/local/bin/s3d /usr/local/bin/s3dctl
```

### Verify binary

```bash
s3d --version
# Expected: s3d version 1.0.0
```

---

## 13. s3d configuration

```bash
mkdir -p /etc/plusclouds
```

Create `/etc/plusclouds/agent.yaml`. Replace the four `REPLACE-*` values with
credentials provided by the PlusClouds platform for this server.

```yaml
nats:
  connection_type: websocket
  url: nats://nats.plusclouds.com:4222
  websocket_url: wss://nats.plusclouds.com:443

  # Provided by the platform at provisioning time
  agent_uuid: "REPLACE-WITH-SERVER-UUID"
  api_key:    "REPLACE-WITH-API-KEY"

  subject_type: s3
  max_reconnects: -1
  reconnect_wait: 5s

agent:
  type: s3
  heartbeat_interval: 30s
  telemetry_interval: 30s

  allowed_operations:
    - agent.allowed_operations
    - services.list
    - services.get
    - services.start
    - services.stop
    - services.restart
    - services.reload
    - services.enable
    - services.disable
    - system.info
    - system.metrics
    - system.cpu
    - system.memory
    - system.disk
    - system.network
    - system.update
    - telemetry.set_interval
    - s3.cluster.status
    - s3.bucket.stats
    - full_sync
    - bucket_create
    - bucket_update
    - bucket_delete
    - reconcile
    - worm_bucket_create
    - worm_bucket_update
    - worm_bucket_delete
    - iam_create
    - iam_delete
    - s3.iam.list
    - s3.customer.block
    - s3.customer.unblock
    - s3.blocked.list
    - exec

  allowed_commands:
    - /usr/bin/journalctl
    - /usr/bin/df
    - /usr/bin/free

s3:
  master_url: http://localhost:9333
  volume_url: http://localhost:8080
  filer_url:  http://localhost:8888
  s3_url:     http://localhost:8333

  iam_file:        /etc/seaweedfs/s3.json
  weed_s3_service: weed-s3

  nginx_blocked_keys_file: /etc/nginx/conf.d/s3_blocked_keys.conf
  # Access log parsed by s3d for per-bucket traffic accounting (upload/download bytes).
  # Must match the access_log path in the Nginx site config.
  nginx_access_log: /var/log/nginx/s3_access.log

  capacity_warn_pct:     80.0
  capacity_critical_pct: 90.0

  # Admin credential used by s3d to sign bucket create/delete API requests.
  # s3d seeds this identity into s3.json on first start (EnsureAdmin).
  # Generate a random key pair — keep them consistent across restarts.
  admin_access_key: "REPLACE-WITH-ADMIN-ACCESS-KEY"
  admin_secret_key: "REPLACE-WITH-ADMIN-SECRET-KEY"
  s3_region: us-east-1

iso:
  mount_path: /media/plusclouds-config

log:
  level: info
  format: json
  file: /var/log/plusclouds/agent.log

autoheal:
  enabled: true
  restart_delay: 10s
```

Secure the config file (contains the API key and S3 admin secret):

```bash
chmod 640 /etc/plusclouds/agent.yaml
```

### Generating admin credentials

If you don't have values for `admin_access_key` / `admin_secret_key` yet:

```bash
# Access key: 16 alphanumeric characters
LC_ALL=C tr -dc 'a-z0-9' </dev/urandom | head -c 16
# Secret key: 40 alphanumeric characters
LC_ALL=C tr -dc 'A-Za-z0-9' </dev/urandom | head -c 40
```

`s3d` calls `EnsureAdmin()` on every start. It reads `s3.json`, and if the
`admin_access_key` is not already present, it appends the admin identity and
restarts `weed-s3`. On subsequent starts it is a no-op.

---

## 14. s3d systemd service

Create `/etc/systemd/system/s3d.service`:

```ini
[Unit]
Description=PlusClouds S3 Storage Agent
After=network.target weed-s3.service nginx.service
Wants=weed-master.service weed-volume.service weed-filer.service weed-s3.service

[Service]
Type=simple
User=root
ExecStart=/usr/local/bin/s3d --config /etc/plusclouds/agent.yaml
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=s3d

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable s3d
systemctl start s3d

systemctl status s3d
journalctl -u s3d -n 50 --no-pager
```

Expected log output on a clean start:

```
s3d starting  version=1.0.0 agent_type=s3
modules initialised  allowed_operations=31
s3d started  agent_type=s3
```

After `s3d` starts, it seeds the admin identity into `/etc/seaweedfs/s3.json`
and restarts `weed-s3`. This is expected — wait a few seconds for `weed-s3` to
come back before running smoke tests.

---

## 15. Verify full stack

### Check SeaweedFS topology

```bash
# Confirm the data node is registered and has free volume slots
curl -s http://localhost:9333/vol/status | jq '.Volumes | {Free, Max}'
# Expected: {"Free": 48, "Max": 50}  (or similar — Free must be > 0)

# Confirm volume assignment works for a new collection
curl -s "http://localhost:9333/dir/assign?collection=smoke-test"
# Expected: {"fid":"N,xxxx","url":"SERVER_IP:8080",...}
# If this hangs, Free is 0 — see Troubleshooting below.
```

### Authenticated S3 smoke test

Once `s3d` has started (and thus seeded the admin credential), all bucket
operations require authentication. Use the admin key from `agent.yaml`:

```bash
ADMIN_KEY="REPLACE-WITH-ADMIN-ACCESS-KEY"
ADMIN_SECRET="REPLACE-WITH-ADMIN-SECRET-KEY"

# Install awscli for a clean test (or use any S3-compatible client)
apt-get install -y awscli

aws configure set aws_access_key_id     "$ADMIN_KEY"
aws configure set aws_secret_access_key "$ADMIN_SECRET"
aws configure set default.region        us-east-1

ENDPOINT="http://localhost:8333"

# Create bucket
aws s3 mb s3://smoke-test --endpoint-url "$ENDPOINT"

# Upload object
echo "hello seaweedfs" | aws s3 cp - s3://smoke-test/hello.txt \
  --endpoint-url "$ENDPOINT"

# List bucket
aws s3 ls s3://smoke-test --endpoint-url "$ENDPOINT"
# Expected: hello.txt

# Delete object and bucket
aws s3 rm  s3://smoke-test/hello.txt --endpoint-url "$ENDPOINT"
aws s3 rb  s3://smoke-test           --endpoint-url "$ENDPOINT"
```

### Verify HTTPS endpoint

```bash
# Unauthenticated request — returns 403 fast (not a hang)
curl -s -o /dev/null -w "HTTP %{http_code} in %{time_total}s\n" \
  https://PUBLIC_DOMAIN/smoke-test/probe.txt
# Expected: HTTP 403 in ~0.1s
```

### Verify NATS connectivity

```bash
journalctl -u s3d -n 20 --no-pager | grep -E "started|NATS|error"
```

---

## 16. WORM / Object Lock buckets

WORM (Write Once Read Many) buckets use SeaweedFS's S3 Object Lock support.
Once an object is written under a retention lock, **nobody** (not even the admin)
can delete or overwrite it before the retention period expires.

### Operations

| Operation | Description |
| --- | --- |
| `worm_bucket_create` | Create a new bucket with Object Lock enabled and a default retention policy |
| `worm_bucket_update` | Change the default retention mode or period on an existing WORM bucket |
| `worm_bucket_delete` | Delete an empty WORM bucket (non-empty buckets return an error) |

### Parameters

| Parameter | Type | Required | Description |
| --- | --- | --- | --- |
| `name` | string | yes | Bucket name (lowercase, DNS-compatible) |
| `bucket_id` | string | yes | Platform UUID for this bucket |
| `owner_tenant_id` | string | yes | Tenant UUID that owns the bucket |
| `object_lock_mode` | string | yes | `COMPLIANCE` or `GOVERNANCE` |
| `retention_days` | int | yes | Default retention period in days (minimum 1) |

**COMPLIANCE** — No one can delete or overwrite a locked object before the
retention period expires, including root and the admin key. This is enforced
by SeaweedFS at the S3 API layer.

**GOVERNANCE** — Admin users with the `s3:BypassGovernanceRetention` action
can bypass the lock. Suitable for internal buckets where you need an
administrative escape hatch.

### Example NATS command

```json
{
  "operation": "worm_bucket_create",
  "params": {
    "name":            "legal-archive",
    "bucket_id":       "bkt-00000000-0000-0000-0000-000000000001",
    "owner_tenant_id": "tnt-00000000-0000-0000-0000-000000000001",
    "object_lock_mode": "COMPLIANCE",
    "retention_days":  365
  }
}
```

Expected response:

```json
{
  "bucket": "legal-archive",
  "status": "created",
  "mode":   "COMPLIANCE"
}
```

### Limitations

- Object Lock must be enabled **at bucket creation time**. It cannot be added to
  an existing standard bucket. Use `worm_bucket_create`, not `bucket_create`.
- `worm_bucket_delete` will fail if any objects remain in the bucket — WORM
  objects cannot be force-deleted.
- SeaweedFS 3.x and earlier do not honour Object Lock at the storage layer.
  Run SeaweedFS 4.x (`weed version` to confirm).

---

## 17. Post-install checklist

```
[ ] All four weed-* services are active and enabled at boot
[ ] nginx is active and serving HTTPS on PUBLIC_DOMAIN
[ ] s3d is active and connected to NATS
[ ] vol/status shows Free > 0 (volume slots available)
[ ] dir/assign?collection=test returns a fid without hanging
[ ] Authenticated S3 smoke test passes (create/put/list/delete)
[ ] Firewall allows only 22, 80, 443 — SeaweedFS ports blocked externally
[ ] /etc/plusclouds/agent.yaml has chmod 640 (contains API key + S3 secret)
[ ] /etc/seaweedfs/s3.json has chmod 640, owner seaweedfs:seaweedfs
[ ] Log rotation configured for /var/log/plusclouds/agent.log
[ ] Let's Encrypt certificate is valid and auto-renewal is working
[ ] Platform has received at least one heartbeat from this agent
```

### Log rotation

Create `/etc/logrotate.d/s3d`:

```
/var/log/plusclouds/agent.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
```

---

## Troubleshooting

### PUT requests hang indefinitely (no error, no response)

**Symptom:** Authenticated S3 PUT requests time out after 30 s with 0 bytes
received. Unauthenticated requests return 403 immediately.

**Cause:** The master has 0 free volume slots. It cannot allocate a new volume
for the bucket's collection, so the request blocks waiting for a slot that
never comes.

```bash
# Diagnose
curl -s http://localhost:9333/vol/status | jq '.Volumes | {Free, Max}'
# If Free is 0, this is your problem.

# Fix: increase max volumes and restart the volume server
sed -i 's/-max=0/-max=50/' /etc/systemd/system/weed-volume.service
systemctl daemon-reload
systemctl restart weed-volume
sleep 3

# Verify
curl -s http://localhost:9333/vol/status | jq '.Volumes | {Free, Max}'
# Expected: Free > 0
```

### weed-s3 crashes after s3.json is written

**Symptom:** `weed-s3` exits with `permission denied` after `s3d` writes to
`s3.json`.

**Cause:** The file was created or replaced as `root:root`. The `weed-s3` process
runs as the `seaweedfs` user and cannot read it.

```bash
chown seaweedfs:seaweedfs /etc/seaweedfs/s3.json
chmod 640 /etc/seaweedfs/s3.json
systemctl restart weed-s3
```

### SeaweedFS won't start

```bash
# Check if a previous instance left a lock file
ls /data/seaweedfs/master/*.lock
rm -f /data/seaweedfs/master/*.lock

# Check disk permissions
ls -la /data/seaweedfs/
# Must be owned by seaweedfs:seaweedfs

systemctl start weed-master
journalctl -u weed-master -f
```

### systemctl reload weed-s3 exits with code 3

`weed-s3` has no reload handler. Always use `systemctl restart weed-s3` after
changing `s3.json`. The `s3d` agent handles this automatically.

### s3d won't connect to NATS

```bash
# Verify connectivity to the NATS WebSocket endpoint
curl -I https://nats.plusclouds.com:443 2>&1 | head -5

# Check the agent UUID and API key in the config
grep -E "agent_uuid|api_key" /etc/plusclouds/agent.yaml

journalctl -u s3d -f
```

### Nginx returns 502 for S3 requests

```bash
# Verify the S3 gateway is listening
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8333/
# Expected: 200 or 403

# Check Nginx error log
tail -20 /var/log/nginx/error.log

# Check weed-s3 is running
systemctl status weed-s3
```

### Let's Encrypt challenge fails

The most common cause is Cloudflare proxy being enabled on the DNS record.
Certbot uses HTTP-01 challenge, which requires the request to reach the server
directly, not a Cloudflare proxy IP.

Temporarily set the DNS record to **DNS only** (grey cloud in Cloudflare), wait
for propagation, then re-run certbot:

```bash
dig +short PUBLIC_DOMAIN
# Must return SERVER_IP, not a Cloudflare IP (188.114.x.x / 104.x.x.x)

certbot --nginx -d PUBLIC_DOMAIN --non-interactive --agree-tos --email admin@example.com
```
