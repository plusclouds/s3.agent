#!/usr/bin/env bash
# deploy.sh — Install and configure the full s3d stack on Ubuntu 24.04 LTS.
#
# Usage:
#   sudo bash deploy.sh [OPTIONS]
#
# Options:
#   --server-ip   IP       Internal IP of this server (required)
#   --domain      DOMAIN   Public S3 domain name, e.g. s3.example.com (required)
#   --agent-uuid  UUID     Agent UUID from the PlusClouds platform (required)
#   --api-key     KEY      Agent API key from the PlusClouds platform (required)
#   --email       EMAIL    Email for Let's Encrypt notifications (required)
#   --skip-tls             Skip Let's Encrypt — use when testing without a real domain
#   --skip-agent           Skip s3d installation (SeaweedFS + Nginx only)
#   --s3d-binary  PATH     Path to a local s3d binary (skips download)
#   --s3dctl-binary PATH   Path to a local s3dctl binary (skips download)
#
# Example:
#   sudo bash deploy.sh \
#     --server-ip   10.0.0.10 \
#     --domain      s3.example.com \
#     --agent-uuid  d6199047-322a-4845-bdea-1d44dd1b49e5 \
#     --api-key     7Qic72PeIToUWqdiIEcfPxwgVBuNNcLRBjGxuoGEJefeu92NTqanK72JlBeeXBJ3 \
#     --email       admin@example.com

set -euo pipefail

# ── colours ──────────────────────────────────────────────────────────────────
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BOLD='\033[1m'; RESET='\033[0m'

info()    { echo -e "${GREEN}[INFO]${RESET}  $*"; }
warn()    { echo -e "${YELLOW}[WARN]${RESET}  $*"; }
error()   { echo -e "${RED}[ERROR]${RESET} $*" >&2; }
section() { echo -e "\n${BOLD}━━━  $*  ━━━${RESET}"; }
die()     { error "$*"; exit 1; }

# ── defaults ─────────────────────────────────────────────────────────────────
SERVER_IP=""
PUBLIC_DOMAIN=""
AGENT_UUID=""
API_KEY=""
LETSENCRYPT_EMAIL=""
SKIP_TLS=false
SKIP_AGENT=false
S3D_BINARY=""
S3DCTL_BINARY=""

WEED_INSTALL_DIR="/usr/local/bin"
WEED_DATA_DIR="/data/seaweedfs"
WEED_CONFIG_DIR="/etc/seaweedfs"
NGINX_CONF_DIR="/etc/nginx"
AGENT_CONFIG_DIR="/etc/plusclouds"
AGENT_LOG_DIR="/var/log/plusclouds"

# ── argument parsing ──────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --server-ip)    SERVER_IP="$2";          shift 2 ;;
    --domain)       PUBLIC_DOMAIN="$2";      shift 2 ;;
    --agent-uuid)   AGENT_UUID="$2";         shift 2 ;;
    --api-key)      API_KEY="$2";            shift 2 ;;
    --email)        LETSENCRYPT_EMAIL="$2";  shift 2 ;;
    --skip-tls)     SKIP_TLS=true;           shift   ;;
    --skip-agent)   SKIP_AGENT=true;         shift   ;;
    --s3d-binary)   S3D_BINARY="$2";         shift 2 ;;
    --s3dctl-binary) S3DCTL_BINARY="$2";     shift 2 ;;
    *) die "Unknown option: $1" ;;
  esac
done

# ── validation ────────────────────────────────────────────────────────────────
[[ $EUID -eq 0 ]] || die "This script must be run as root (use sudo)."
[[ -n "$SERVER_IP" ]]     || die "--server-ip is required."
[[ -n "$PUBLIC_DOMAIN" ]] || die "--domain is required."

if [[ "$SKIP_AGENT" == false ]]; then
  [[ -n "$AGENT_UUID" ]] || die "--agent-uuid is required (or pass --skip-agent)."
  [[ -n "$API_KEY" ]]    || die "--api-key is required (or pass --skip-agent)."
fi

if [[ "$SKIP_TLS" == false ]]; then
  [[ -n "$LETSENCRYPT_EMAIL" ]] || die "--email is required for Let's Encrypt (or pass --skip-tls)."
fi

# ── helpers ───────────────────────────────────────────────────────────────────
check_http() {
  local url="$1" expected="${2:-200}"
  local code
  code=$(curl -s -o /dev/null -w "%{http_code}" --max-time 5 "$url" 2>/dev/null || true)
  [[ "$code" == "$expected" ]]
}

wait_for_http() {
  local url="$1" label="$2" retries=15 wait=2
  info "Waiting for $label to become reachable..."
  for i in $(seq 1 $retries); do
    if check_http "$url"; then
      info "$label is up."
      return 0
    fi
    sleep "$wait"
  done
  die "$label did not become reachable at $url after $((retries * wait))s."
}

# ── step 1: OS packages ───────────────────────────────────────────────────────
section "Step 1 — OS packages"

apt-get update -qq
apt-get install -y -qq \
  curl wget jq unzip \
  nginx certbot python3-certbot-nginx \
  ufw \
  awscli

info "OS packages installed."

# ── step 2: user and directories ─────────────────────────────────────────────
section "Step 2 — Service user and directories"

if ! id seaweedfs &>/dev/null; then
  useradd --system --no-create-home --shell /usr/sbin/nologin seaweedfs
  info "Created user: seaweedfs"
else
  info "User seaweedfs already exists — skipping."
fi

mkdir -p \
  "$WEED_DATA_DIR/master" \
  "$WEED_DATA_DIR/volumes" \
  "$WEED_DATA_DIR/filer" \
  "$WEED_CONFIG_DIR" \
  "$NGINX_CONF_DIR/conf.d" \
  "$AGENT_CONFIG_DIR" \
  "$AGENT_LOG_DIR"

chown -R seaweedfs:seaweedfs "$WEED_DATA_DIR" "$WEED_CONFIG_DIR"
info "Directories created."

# ── step 3: firewall ─────────────────────────────────────────────────────────
section "Step 3 — Firewall (UFW)"

ufw --force reset   >/dev/null
ufw default deny incoming >/dev/null
ufw default allow outgoing >/dev/null
ufw allow 22/tcp   comment "SSH"    >/dev/null
ufw allow 80/tcp   comment "HTTP"   >/dev/null
ufw allow 443/tcp  comment "HTTPS"  >/dev/null
ufw --force enable >/dev/null

info "Firewall configured. Active rules:"
ufw status numbered | grep -v "^$" | sed 's/^/  /'

# ── step 4: install SeaweedFS ─────────────────────────────────────────────────
section "Step 4 — Install SeaweedFS"

if [[ -x "$WEED_INSTALL_DIR/weed" ]]; then
  CURRENT_VER=$("$WEED_INSTALL_DIR/weed" version 2>/dev/null | grep -oP '[\d]+\.[\d]+\.[\d]+' | head -1 || echo "unknown")
  info "SeaweedFS already installed: $CURRENT_VER — skipping download."
else
  WEED_VERSION=$(curl -s https://api.github.com/repos/seaweedfs/seaweedfs/releases/latest \
    | jq -r '.tag_name' | tr -d 'v')
  info "Downloading SeaweedFS $WEED_VERSION..."

  TMPDIR=$(mktemp -d)
  trap 'rm -rf "$TMPDIR"' EXIT

  wget -q "https://github.com/seaweedfs/seaweedfs/releases/download/${WEED_VERSION}/linux_amd64_large_disk.tar.gz" \
    -O "$TMPDIR/weed.tar.gz"
  tar -xzf "$TMPDIR/weed.tar.gz" -C "$TMPDIR"
  mv "$TMPDIR/weed" "$WEED_INSTALL_DIR/weed"
  chmod +x "$WEED_INSTALL_DIR/weed"

  info "SeaweedFS $WEED_VERSION installed at $WEED_INSTALL_DIR/weed."
fi

# ── step 5: SeaweedFS config files ────────────────────────────────────────────
section "Step 5 — SeaweedFS configuration"

# filer metadata backend (LevelDB, zero dependencies)
if [[ ! -f "$WEED_CONFIG_DIR/filer.toml" ]]; then
  cat > "$WEED_CONFIG_DIR/filer.toml" << EOF
[leveldb2]
enabled = true
dir = "$WEED_DATA_DIR/filer"
EOF
  chown seaweedfs:seaweedfs "$WEED_CONFIG_DIR/filer.toml"
  info "Created filer.toml"
else
  info "filer.toml already exists — skipping."
fi

# initial IAM file (s3d manages this at runtime)
if [[ ! -f "$WEED_CONFIG_DIR/s3.json" ]]; then
  echo '{"identities":[]}' > "$WEED_CONFIG_DIR/s3.json"
  chown seaweedfs:seaweedfs "$WEED_CONFIG_DIR/s3.json"
  chmod 640 "$WEED_CONFIG_DIR/s3.json"
  info "Created s3.json (empty identities)"
else
  info "s3.json already exists — skipping."
fi

# initial Nginx blocked-keys include (s3d manages this at runtime)
if [[ ! -f "$NGINX_CONF_DIR/conf.d/s3_blocked_keys.conf" ]]; then
  echo "# Managed by s3d — do not edit manually" \
    > "$NGINX_CONF_DIR/conf.d/s3_blocked_keys.conf"
  info "Created s3_blocked_keys.conf"
fi

# ── step 6: systemd units — SeaweedFS ────────────────────────────────────────
section "Step 6 — systemd units (SeaweedFS)"

write_unit() {
  local name="$1" content="$2"
  echo "$content" > "/etc/systemd/system/$name"
  info "Written /etc/systemd/system/$name"
}

write_unit "weed-master.service" "[Unit]
Description=SeaweedFS Master
After=network.target
Wants=network.target

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=$WEED_INSTALL_DIR/weed master \\
  -ip=$SERVER_IP \\
  -port=9333 \\
  -mdir=$WEED_DATA_DIR/master \\
  -defaultReplication=001 \\
  -volumeSizeLimitMB=30000
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-master

[Install]
WantedBy=multi-user.target"

write_unit "weed-volume.service" "[Unit]
Description=SeaweedFS Volume
After=weed-master.service
Requires=weed-master.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=$WEED_INSTALL_DIR/weed volume \\
  -ip=$SERVER_IP \\
  -port=8080 \\
  -mserver=$SERVER_IP:9333 \\
  -dir=$WEED_DATA_DIR/volumes \\
  -max=0 \\
  -minFreeSpacePercent=5
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-volume

[Install]
WantedBy=multi-user.target"

write_unit "weed-filer.service" "[Unit]
Description=SeaweedFS Filer
After=weed-master.service
Requires=weed-master.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=$WEED_INSTALL_DIR/weed filer \\
  -ip=$SERVER_IP \\
  -port=8888 \\
  -master=$SERVER_IP:9333 \\
  -defaultStoreDir=$WEED_DATA_DIR/filer
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-filer

[Install]
WantedBy=multi-user.target"

write_unit "weed-s3.service" "[Unit]
Description=SeaweedFS S3 Gateway
After=weed-filer.service
Requires=weed-filer.service

[Service]
Type=simple
User=seaweedfs
Group=seaweedfs
ExecStart=$WEED_INSTALL_DIR/weed s3 \\
  -ip=127.0.0.1 \\
  -port=8333 \\
  -filer=$SERVER_IP:8888 \\
  -config=$WEED_CONFIG_DIR/s3.json
Restart=on-failure
RestartSec=10
StandardOutput=journal
StandardError=journal
SyslogIdentifier=weed-s3

[Install]
WantedBy=multi-user.target"

systemctl daemon-reload
systemctl enable weed-master weed-volume weed-filer weed-s3
info "SeaweedFS units enabled."

# ── step 7: start and verify SeaweedFS ───────────────────────────────────────
section "Step 7 — Start SeaweedFS"

start_and_verify() {
  local svc="$1" url="$2" label="$3"
  if systemctl is-active --quiet "$svc"; then
    info "$svc already running — restarting to pick up new config."
    systemctl restart "$svc"
  else
    systemctl start "$svc"
  fi
  wait_for_http "$url" "$label"
}

start_and_verify weed-master "http://localhost:9333/cluster/status" "weed-master"
start_and_verify weed-volume "http://localhost:8080/status"         "weed-volume"
start_and_verify weed-filer  "http://localhost:8888/"               "weed-filer"
start_and_verify weed-s3     "http://localhost:8333/"               "weed-s3"

info "All SeaweedFS components are up."

# ── step 8: Nginx configuration ───────────────────────────────────────────────
section "Step 8 — Nginx"

rm -f "$NGINX_CONF_DIR/sites-enabled/default"

if [[ "$SKIP_TLS" == true ]]; then
  # Plain HTTP config — for local testing only
  cat > "$NGINX_CONF_DIR/sites-available/s3" << EOF
server {
    listen 80;
    server_name $PUBLIC_DOMAIN;

    include $NGINX_CONF_DIR/conf.d/s3_blocked_keys.conf;

    client_max_body_size 50G;
    client_body_timeout  300s;

    access_log /var/log/nginx/s3_access.log;

    location / {
        proxy_pass         http://127.0.0.1:8333;
        proxy_set_header   Host              \$host;
        proxy_set_header   X-Real-IP         \$remote_addr;
        proxy_set_header   X-Forwarded-For   \$proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto \$scheme;
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      300s;
        proxy_send_timeout      300s;
    }
}
EOF
  warn "TLS skipped — Nginx configured for HTTP only. Do not use in production."
else
  # HTTPS config (certificate written by certbot below)
  cat > "$NGINX_CONF_DIR/sites-available/s3" << EOF
server {
    listen 80;
    server_name $PUBLIC_DOMAIN;
    return 301 https://\$host\$request_uri;
}

server {
    listen 443 ssl;
    server_name $PUBLIC_DOMAIN;

    ssl_certificate     /etc/letsencrypt/live/$PUBLIC_DOMAIN/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/$PUBLIC_DOMAIN/privkey.pem;
    ssl_protocols       TLSv1.2 TLSv1.3;
    ssl_ciphers         HIGH:!aNULL:!MD5;
    ssl_session_cache   shared:SSL:10m;
    add_header Strict-Transport-Security "max-age=31536000" always;

    include $NGINX_CONF_DIR/conf.d/s3_blocked_keys.conf;

    client_max_body_size 50G;
    client_body_timeout  300s;

    log_format s3_access '\$time_iso8601 \$http_authorization '
                         '\$request_method "\$request_uri" '
                         '\$status \$bytes_sent \$request_length '
                         '"\$http_user_agent"';
    access_log /var/log/nginx/s3_access.log s3_access;

    location / {
        proxy_pass         http://127.0.0.1:8333;
        proxy_set_header   Host              \$host;
        proxy_set_header   X-Real-IP         \$remote_addr;
        proxy_set_header   X-Forwarded-For   \$proxy_add_x_forwarded_for;
        proxy_set_header   X-Forwarded-Proto \$scheme;
        proxy_request_buffering off;
        proxy_buffering         off;
        proxy_read_timeout      300s;
        proxy_send_timeout      300s;
    }
}
EOF
fi

ln -sf "$NGINX_CONF_DIR/sites-available/s3" "$NGINX_CONF_DIR/sites-enabled/s3"

nginx -t || die "Nginx config test failed. Check $NGINX_CONF_DIR/sites-available/s3"
systemctl enable nginx
systemctl reload nginx || systemctl start nginx
info "Nginx configured and running."

# ── step 9: Let's Encrypt TLS ────────────────────────────────────────────────
if [[ "$SKIP_TLS" == false ]]; then
  section "Step 9 — Let's Encrypt TLS"

  if [[ -f "/etc/letsencrypt/live/$PUBLIC_DOMAIN/fullchain.pem" ]]; then
    info "Certificate for $PUBLIC_DOMAIN already exists — skipping certbot."
  else
    certbot --nginx \
      -d "$PUBLIC_DOMAIN" \
      --non-interactive \
      --agree-tos \
      --email "$LETSENCRYPT_EMAIL" \
      --redirect
    info "Certificate issued for $PUBLIC_DOMAIN."
  fi

  systemctl reload nginx
fi

# ── step 10: install s3d agent ────────────────────────────────────────────────
if [[ "$SKIP_AGENT" == false ]]; then
  section "Step 10 — Install s3d agent"

  # Binary — use local path if provided, otherwise download latest release
  if [[ -n "$S3D_BINARY" ]]; then
    cp "$S3D_BINARY" /usr/local/bin/s3d
    info "Installed s3d from local path: $S3D_BINARY"
  elif [[ ! -x /usr/local/bin/s3d ]]; then
    die "No s3d binary found at /usr/local/bin/s3d and --s3d-binary was not provided.
Build it first with:  make build
Then pass:            --s3d-binary bin/s3d.linux"
  else
    info "s3d already present at /usr/local/bin/s3d — skipping install."
  fi

  if [[ -n "$S3DCTL_BINARY" ]]; then
    cp "$S3DCTL_BINARY" /usr/local/bin/s3dctl
    info "Installed s3dctl from local path: $S3DCTL_BINARY"
  elif [[ -x /usr/local/bin/s3dctl ]]; then
    info "s3dctl already present — skipping install."
  fi

  chmod +x /usr/local/bin/s3d /usr/local/bin/s3dctl 2>/dev/null || true

  # ── agent config ──────────────────────────────────────────────────────────
  if [[ -f "$AGENT_CONFIG_DIR/agent.yaml" ]]; then
    warn "$AGENT_CONFIG_DIR/agent.yaml already exists — not overwriting."
    warn "To regenerate it, delete the file and re-run this script."
  else
    cat > "$AGENT_CONFIG_DIR/agent.yaml" << EOF
nats:
  connection_type: websocket
  url: nats://nats.plusclouds.com:4222
  websocket_url: wss://nats.plusclouds.com:443
  agent_uuid: "$AGENT_UUID"
  api_key:    "$API_KEY"
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
  iam_file:        $WEED_CONFIG_DIR/s3.json
  weed_s3_service: weed-s3
  nginx_blocked_keys_file: $NGINX_CONF_DIR/conf.d/s3_blocked_keys.conf
  capacity_warn_pct:     80.0
  capacity_critical_pct: 90.0

iso:
  mount_path: /media/plusclouds-config

log:
  level: info
  format: json
  file: $AGENT_LOG_DIR/agent.log

autoheal:
  enabled: true
  restart_delay: 10s
EOF
    chmod 640 "$AGENT_CONFIG_DIR/agent.yaml"
    info "Written $AGENT_CONFIG_DIR/agent.yaml"
  fi

  # ── s3d systemd unit ──────────────────────────────────────────────────────
  cat > /etc/systemd/system/s3d.service << 'UNIT'
[Unit]
Description=PlusClouds S3 Storage Agent
Documentation=https://github.com/plusclouds/s3.agent
After=network-online.target weed-s3.service nginx.service
Wants=network-online.target weed-master.service weed-volume.service weed-filer.service weed-s3.service

[Service]
Type=simple
User=root
Group=root

ExecStartPre=/bin/mkdir -p /var/log/plusclouds
ExecStartPre=/bin/chmod 0750 /var/log/plusclouds
ExecStartPre=/bin/sleep 5

ExecStart=/usr/local/bin/s3d --config /etc/plusclouds/agent.yaml

Restart=always
RestartSec=5
StandardOutput=journal
StandardError=journal
SyslogIdentifier=s3d

EnvironmentFile=-/etc/plusclouds/environment

NoNewPrivileges=yes
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/plusclouds /etc/plusclouds /etc/seaweedfs /etc/nginx/conf.d
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictRealtime=true

LimitNOFILE=65536
LimitNPROC=4096

[Install]
WantedBy=multi-user.target
UNIT

  systemctl daemon-reload
  systemctl enable s3d
  systemctl start s3d

  # Give the agent a moment to connect
  sleep 3
  if systemctl is-active --quiet s3d; then
    info "s3d agent started successfully."
  else
    error "s3d failed to start. Check logs with:  journalctl -u s3d -n 50 --no-pager"
  fi
fi

# ── step 11: log rotation ─────────────────────────────────────────────────────
section "Step 11 — Log rotation"

cat > /etc/logrotate.d/s3d << 'EOF'
/var/log/plusclouds/agent.log {
    daily
    rotate 14
    compress
    delaycompress
    missingok
    notifempty
    copytruncate
}
EOF
info "Log rotation configured."

# ── step 12: preflight check ──────────────────────────────────────────────────
section "Step 12 — Preflight verification"

if [[ "$SKIP_AGENT" == false ]] && command -v s3dctl &>/dev/null; then
  info "Running s3dctl check..."
  s3dctl --config "$AGENT_CONFIG_DIR/agent.yaml" check || true
else
  info "Manual verification (s3dctl not available or --skip-agent set):"
  echo ""
  printf "  %-40s " "weed-master service"
  systemctl is-active --quiet weed-master && echo -e "${GREEN}active${RESET}" || echo -e "${RED}inactive${RESET}"
  printf "  %-40s " "weed-volume service"
  systemctl is-active --quiet weed-volume && echo -e "${GREEN}active${RESET}" || echo -e "${RED}inactive${RESET}"
  printf "  %-40s " "weed-filer service"
  systemctl is-active --quiet weed-filer  && echo -e "${GREEN}active${RESET}" || echo -e "${RED}inactive${RESET}"
  printf "  %-40s " "weed-s3 service"
  systemctl is-active --quiet weed-s3     && echo -e "${GREEN}active${RESET}" || echo -e "${RED}inactive${RESET}"
  printf "  %-40s " "nginx service"
  systemctl is-active --quiet nginx       && echo -e "${GREEN}active${RESET}" || echo -e "${RED}inactive${RESET}"
  printf "  %-40s " "master API"
  check_http "http://localhost:9333/cluster/status" && echo -e "${GREEN}reachable${RESET}" || echo -e "${RED}unreachable${RESET}"
  printf "  %-40s " "volume API"
  check_http "http://localhost:8080/status"         && echo -e "${GREEN}reachable${RESET}" || echo -e "${RED}unreachable${RESET}"
  printf "  %-40s " "filer API"
  check_http "http://localhost:8888/"               && echo -e "${GREEN}reachable${RESET}" || echo -e "${RED}unreachable${RESET}"
  printf "  %-40s " "s3 gateway API"
  check_http "http://localhost:8333/"               && echo -e "${GREEN}reachable${RESET}" || echo -e "${RED}unreachable${RESET}"
  echo ""
fi

# ── done ──────────────────────────────────────────────────────────────────────
echo ""
echo -e "${BOLD}${GREEN}━━━  Deployment complete  ━━━${RESET}"
echo ""
echo "  S3 endpoint:    https://$PUBLIC_DOMAIN"
echo "  Agent config:   $AGENT_CONFIG_DIR/agent.yaml"
echo "  Agent logs:     journalctl -u s3d -f"
echo "  SeaweedFS logs: journalctl -u weed-master -u weed-volume -u weed-filer -u weed-s3 -f"
echo ""
if [[ "$SKIP_AGENT" == false ]]; then
  echo "  Next steps:"
  echo "    1. Confirm the platform received a heartbeat from agent $AGENT_UUID"
  echo "    2. Run a full_sync command from the platform to provision initial state"
  echo "    3. Test S3 access: s3dctl --config $AGENT_CONFIG_DIR/agent.yaml cluster status"
  echo ""
fi
