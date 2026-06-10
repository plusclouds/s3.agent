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
16. [Post-install checklist](#16-post-install-checklist)

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
SERVER_IP=10.0.0.10          # Internal IP of this server
PUBLIC_DOMAIN=s3.example.com # Public S3 endpoint hostname
```

---

## 2. OS baseline

```bash
# Update all packages
apt-get update && apt-get upgrade -y

# Install utilities needed throughout this guide
apt-get install -y \
  curl wget jq unzip \
  nginx certbot python3-certbot-nginx \
  ufw

# Set the server hostname (optional but helps with logs)
hostnamectl set-hostname storage01
```

---

## 3. Create service user and directories

SeaweedFS runs as a dedicated non-root user. The `s3d` agent runs as root
(required for systemd D-Bus access).

```bash
# Create the seaweedfs system user (no login shell, no home directory)
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
# Enable UFW
ufw --force enable

# Allow SSH (do this first or you will lock yourself out)
ufw allow 22/tcp comment "SSH"

# Allow public HTTP/HTTPS for S3 clients (via Nginx)
ufw allow 80/tcp  comment "HTTP → HTTPS redirect"
ufw allow 443/tcp comment "HTTPS S3 endpoint"

# SeaweedFS ports — localhost only, no public rules needed
# These are blocked by default; the services bind to 127.0.0.1 / SERVER_IP

# Check status
ufw status verbose
```

> **Important:** Never open ports 8080, 8333, 8888, or 9333 in the firewall.
> All client traffic must go through Nginx on port 443.

---

## 5. Install SeaweedFS

SeaweedFS ships as a single static binary called `weed`.

```bash
# Find the latest release version
WEED_VERSION=$(curl -s https://api.github.com/repos/seaweedfs/seaweedfs/releases/latest \
  | jq -r '.tag_name' | tr -d 'v')

echo "Installing SeaweedFS $WEED_VERSION"

# Download the Linux amd64 build
cd /tmp
wget "https://github.com/seaweedfs/seaweedfs/releases/download/${WEED_VERSION}/linux_amd64_large_disk.tar.gz" \
  -O weed.tar.gz

# Extract and install
tar -xzf weed.tar.gz
mv weed /usr/local/bin/weed
chmod +x /usr/local/bin/weed

# Verify
weed version
```

> The `large_disk` build supports volumes larger than 30 GB. Use it unless
> your server has only small disks.

---

## 6. SeaweedFS configuration files

### Filer metadata backend

The filer needs a backend for its namespace metadata. For a single-node setup,
LevelDB is the simplest option (zero dependencies).

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
identities list — `s3d` will manage this file at runtime.

```bash
cat > /etc/seaweedfs/s3.json << 'EOF'
{
  "identities": []
}
EOF

chown seaweedfs:seaweedfs /etc/seaweedfs/s3.json
chmod 640 /etc/seaweedfs/s3.json
```

### Initial Nginx blocked-keys include

```bash
cat > /etc/nginx/conf.d/s3_blocked_keys.conf << 'EOF'
# Managed by s3d — do not edit manually
EOF
```

---

## 7. systemd service units

Copy each file exactly as shown. Replace `SERVER_IP` with your server's IP.

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
  -defaultReplication=001 \
  -volumeSizeLimitMB=30000
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-master

[Install]
WantedBy=multi-user.target
```

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
  -max=0 \
  -minFreeSpacePercent=5
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-volume

[Install]
WantedBy=multi-user.target
```

> `-max=0` lets the volume server create as many volumes as needed.
> `-minFreeSpacePercent=5` stops accepting writes at 95% full.

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
  -ip=127.0.0.1 \
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

> The S3 gateway binds to `127.0.0.1` only — all external traffic goes through Nginx.

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
# Expected: {"IsLeader":true,"Leader":"SERVER_IP:9333","Peers":null}

# Start volume
systemctl start weed-volume
sleep 3
curl -s http://localhost:8080/status | jq .
# Expected: {"Version":"...","Volumes":null,...}

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

# Check all four are active
systemctl is-active weed-master weed-volume weed-filer weed-s3
# Expected: active active active active
```

Check logs if any service fails to start:

```bash
journalctl -u weed-master -n 50 --no-pager
journalctl -u weed-volume -n 50 --no-pager
journalctl -u weed-filer  -n 50 --no-pager
journalctl -u weed-s3     -n 50 --no-pager
```

---

## 9. Install Nginx

```bash
# Install (already included in section 2 — skip if done)
apt-get install -y nginx

# Remove the default site
rm -f /etc/nginx/sites-enabled/default

# Enable and start
systemctl enable nginx
systemctl start nginx
```

---

## 10. TLS certificate (Let's Encrypt)

Skip this section if you already have a certificate or are testing without TLS.

```bash
# Replace s3.example.com with your actual domain
certbot --nginx -d s3.example.com \
  --non-interactive \
  --agree-tos \
  --email admin@example.com

# Test auto-renewal
certbot renew --dry-run
```

Certbot installs a cron job that renews certificates automatically.

---

## 11. Nginx site configuration

Create `/etc/nginx/sites-available/s3`:

```nginx
# HTTP → HTTPS redirect
server {
    listen 80;
    server_name s3.example.com;
    return 301 https://$host$request_uri;
}

server {
    listen 443 ssl;
    server_name s3.example.com;

    # TLS (Certbot fills these in; adjust paths if using your own cert)
    ssl_certificate     /etc/letsencrypt/live/s3.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/s3.example.com/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;
    ssl_session_cache   shared:SSL:10m;
    add_header Strict-Transport-Security "max-age=31536000" always;

    # Blocked access keys (managed by s3d)
    include /etc/nginx/conf.d/s3_blocked_keys.conf;

    # Upload limits
    client_max_body_size 50G;
    client_body_timeout  300s;

    # Access logging for bandwidth metering
    log_format s3_access '$time_iso8601 $http_authorization '
                         '$request_method "$request_uri" '
                         '$status $bytes_sent $request_length '
                         '"$http_user_agent"';
    access_log /var/log/nginx/s3_access.log s3_access;

    location / {
        proxy_pass         http://127.0.0.1:8333;
        proxy_set_header   Host              $host;
        proxy_set_header   X-Real-IP         $remote_addr;
        proxy_set_header   X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto $scheme;

        # Required for streaming large uploads — do not buffer
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      300s;
        proxy_send_timeout      300s;
    }
}
```

Enable and test:

```bash
ln -s /etc/nginx/sites-available/s3 /etc/nginx/sites-enabled/s3

nginx -t
# Expected: syntax is ok / test is successful

systemctl reload nginx
```

---

## 12. Install s3d agent

### Option A — pre-built binary (recommended)

Download the latest release binary from the PlusClouds internal registry:

```bash
# Download
curl -L https://releases.internal.plusclouds.com/s3d/latest/s3d.linux \
  -o /usr/local/bin/s3d
chmod +x /usr/local/bin/s3d

# Also install the CLI tool
curl -L https://releases.internal.plusclouds.com/s3d/latest/s3dctl.linux \
  -o /usr/local/bin/s3dctl
chmod +x /usr/local/bin/s3dctl
```

### Option B — build from source

Requires Go 1.22+:

```bash
apt-get install -y golang-go

git clone https://github.com/plusclouds/s3.agent.git /opt/s3.agent
cd /opt/s3.agent

make build
# Outputs: bin/s3d.linux, bin/s3dctl.linux

cp bin/s3d.linux    /usr/local/bin/s3d
cp bin/s3dctl.linux /usr/local/bin/s3dctl
chmod +x /usr/local/bin/s3d /usr/local/bin/s3dctl
```

### Verify binary

```bash
s3d --version
# Expected: s3d version 1.0.0

s3dctl --help
```

---

## 13. s3d configuration

Create the config directory and write the agent configuration:

```bash
mkdir -p /etc/plusclouds
```

Create `/etc/plusclouds/agent.yaml` — replace the `agent_uuid` and `api_key`
with the values provisioned by the PlusClouds platform for this server:

```yaml
nats:
  connection_type: websocket
  url: nats://nats.plusclouds.com:4222
  websocket_url: wss://nats.plusclouds.com:443

  # Provided by the platform at provisioning time
  agent_uuid: "REPLACE-WITH-SERVER-UUID"
  api_key:    "REPLACE-WITH-API-KEY"

  max_reconnects: -1
  reconnect_wait: 5s

agent:
  type: storage
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
    - bucket_delete
    - reconcile
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

  capacity_warn_pct:     80.0
  capacity_critical_pct: 90.0

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

Secure the config file (contains the API key):

```bash
chmod 640 /etc/plusclouds/agent.yaml
```

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

# Prevent the agent from touching the network before SeaweedFS is ready
ExecStartPre=/bin/sleep 5

[Install]
WantedBy=multi-user.target
```

Enable and start:

```bash
systemctl daemon-reload
systemctl enable s3d
systemctl start s3d

# Verify
systemctl status s3d
journalctl -u s3d -n 50 --no-pager
```

Expected log output on a clean start:

```
s3d starting  version=1.0.0 agent_type=storage
modules initialised  allowed_operations=31
s3d started  agent_type=storage
```

---

## 15. Verify full stack

### Preflight check via s3dctl

```bash
s3dctl --config /etc/plusclouds/agent.yaml check
```

Expected output:

```
Check                          Status
────────────────────────────── ──────
weed-master service            ✓
weed-volume service            ✓
weed-filer service             ✓
weed-s3 service                ✓
nginx service                  ✓
master API reachable           ✓
volume API reachable           ✓
filer API reachable            ✓
s3 gateway reachable           ✓
s3.json readable               ✓
s3.json valid JSON             ✓
s3_blocked_keys.conf readable  ✓
nginx conf writable            ✓
```

### Cluster status

```bash
s3dctl --config /etc/plusclouds/agent.yaml cluster status
```

### Verify NATS connectivity

Check the agent log for a successful NATS connection:

```bash
journalctl -u s3d -n 20 --no-pager | grep -E "started|NATS|error"
```

### Quick S3 API smoke test

Create a test bucket and upload a small file directly to the S3 gateway
(bypassing Nginx, for internal validation only):

```bash
# Install the AWS CLI if needed
apt-get install -y awscli

# Configure with a test identity (after creating one via s3dctl iam create)
aws configure set aws_access_key_id     AKIATEST
aws configure set aws_secret_access_key testSecretKey
aws configure set region                us-east-1

# Test against localhost (internal, bypassing Nginx)
aws s3 mb s3://smoke-test \
  --endpoint-url http://localhost:8333

echo "hello world" | aws s3 cp - s3://smoke-test/hello.txt \
  --endpoint-url http://localhost:8333

aws s3 ls s3://smoke-test \
  --endpoint-url http://localhost:8333

aws s3 rb s3://smoke-test --force \
  --endpoint-url http://localhost:8333
```

---

## 16. Post-install checklist

```
[ ] All four weed-* services are active and enabled at boot
[ ] nginx is active and serving HTTPS
[ ] s3d is active and connected to NATS
[ ] s3dctl check passes all 13 checks
[ ] Firewall allows only 22, 80, 443 — SeaweedFS ports are blocked externally
[ ] /etc/plusclouds/agent.yaml has chmod 640 (contains API key)
[ ] /etc/seaweedfs/s3.json has chmod 640 (contains credentials)
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

### s3d won't connect to NATS

```bash
# Verify connectivity to the NATS WebSocket endpoint
curl -I wss://nats.plusclouds.com:443 --http1.1 2>&1 | head -5

# Check the agent UUID and API key in the config
grep -E "agent_uuid|api_key" /etc/plusclouds/agent.yaml

journalctl -u s3d -f
```

### Nginx returns 502 for S3 requests

```bash
# Verify the S3 gateway is listening
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8333/
# Expected: 200

# Check Nginx error log
tail -20 /var/log/nginx/error.log
```

### Permission denied writing s3.json or s3_blocked_keys.conf

`s3d` runs as root. If the files were created with wrong ownership:

```bash
chown root:root /etc/seaweedfs/s3.json
chmod 640 /etc/seaweedfs/s3.json

chown root:root /etc/nginx/conf.d/s3_blocked_keys.conf
chmod 644 /etc/nginx/conf.d/s3_blocked_keys.conf
```
