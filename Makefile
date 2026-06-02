# PlusClouds Ubuntu Agent Makefile

BINARY_AGENT   := s3d
BINARY_CTL     := s3dctl
BINARY_LINUX   := s3d.linux
BINARY_WINDOWS := s3d.windows
CMD_AGENT      := ./cmd/agent
CMD_CTL        := ./cmd/ctl
BUILD_DIR      := ./bin

VERSION        ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# Development build — fast, includes debug info
LDFLAGS        := -ldflags "-X github.com/plusclouds/ubuntu-agent/internal/config.AgentVersion=$(VERSION)"

# Production build — static binary, stripped, no debug info
LDFLAGS_PROD   := -ldflags "-s -w -X github.com/plusclouds/ubuntu-agent/internal/config.AgentVersion=$(VERSION)"

INSTALL_DIR    := /usr/local/bin
SERVICE_DIR    := /etc/systemd/system
CONFIG_DIR     := /etc/plusclouds
LOG_DIR        := /var/log/plusclouds
SEAWEEDFS_CFG  := /etc/seaweedfs
NGINX_CFG      := /etc/nginx/conf.d

.PHONY: all build build-agent build-ctl build-prod build-linux build-windows build-all test lint clean install uninstall package-deb help

## all: build both binaries (development)
all: build

## build: build both binaries for development (current OS/arch)
build: build-agent build-ctl

## build-agent: build the s3d daemon (development)
build-agent:
	@echo "Building $(BINARY_AGENT) (dev)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_AGENT) $(CMD_AGENT)
	@echo "Done: $(BUILD_DIR)/$(BINARY_AGENT)"

## build-ctl: build the s3dctl CLI (development)
build-ctl:
	@echo "Building $(BINARY_CTL) (dev)..."
	@mkdir -p $(BUILD_DIR)
	go build $(LDFLAGS) -o $(BUILD_DIR)/$(BINARY_CTL) $(CMD_CTL)
	@echo "Done: $(BUILD_DIR)/$(BINARY_CTL)"

## build-prod: alias for build-linux (backwards compatibility)
build-prod: build-linux

## build-linux: build production binary for Linux amd64 → bin/s3d.linux
build-linux:
	@echo "Building $(BINARY_LINUX) $(VERSION) (linux/amd64)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -trimpath $(LDFLAGS_PROD) \
		-o $(BUILD_DIR)/$(BINARY_LINUX) $(CMD_AGENT)
	@echo "Done: $(BUILD_DIR)/$(BINARY_LINUX)"
	@echo "Size: $$(du -sh $(BUILD_DIR)/$(BINARY_LINUX) | cut -f1)"
	@file $(BUILD_DIR)/$(BINARY_LINUX)

## build-windows: build production binary for Windows amd64 → bin/s3d.windows
build-windows:
	@echo "Building $(BINARY_WINDOWS) $(VERSION) (windows/amd64)..."
	@mkdir -p $(BUILD_DIR)
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 \
		go build -trimpath $(LDFLAGS_PROD) \
		-o $(BUILD_DIR)/$(BINARY_WINDOWS) $(CMD_AGENT)
	@echo "Done: $(BUILD_DIR)/$(BINARY_WINDOWS)"
	@echo "Size: $$(du -sh $(BUILD_DIR)/$(BINARY_WINDOWS) | cut -f1)"

## build-all: build production binaries for all platforms
build-all: build-linux build-windows
	@echo ""
	@echo "All platform binaries:"
	@ls -lh $(BUILD_DIR)/s3d.*

## test: run all tests with race detector and coverage
test:
	@echo "Running tests..."
	go test -v -race -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out -o coverage.html
	@echo "Coverage report: coverage.html"

## lint: run golangci-lint
lint:
	@echo "Running linter..."
	golangci-lint run ./...

## clean: remove build artifacts
clean:
	@echo "Cleaning..."
	@rm -rf $(BUILD_DIR) dist
	@rm -f coverage.out coverage.html
	@echo "Clean done."

## install: install binaries, systemd unit, and default config (requires root)
install: build-prod
	@echo "Installing $(BINARY_AGENT) to $(INSTALL_DIR)..."
	install -m 0755 $(BUILD_DIR)/$(BINARY_AGENT) $(INSTALL_DIR)/$(BINARY_AGENT)
	@echo "Installing $(BINARY_CTL) to $(INSTALL_DIR)..."
	install -m 0755 $(BUILD_DIR)/$(BINARY_CTL) $(INSTALL_DIR)/$(BINARY_CTL)
	@echo "Installing systemd unit..."
	install -m 0644 systemd/plusclouds-agent.service $(SERVICE_DIR)/s3d.service
	@echo "Creating directories..."
	@mkdir -p $(CONFIG_DIR) $(LOG_DIR)
	@chmod 0750 $(LOG_DIR)
	@if [ ! -f $(CONFIG_DIR)/agent.yaml ]; then \
		install -m 0640 configs/agent.yaml $(CONFIG_DIR)/agent.yaml; \
		echo "Installed default config to $(CONFIG_DIR)/agent.yaml"; \
	else \
		echo "Config already exists at $(CONFIG_DIR)/agent.yaml — skipping."; \
	fi
	systemctl daemon-reload
	@echo ""
	@echo "Install complete. Next steps:"
	@echo "  1. Edit $(CONFIG_DIR)/agent.yaml (set nats.agent_uuid and nats.api_key)"
	@echo "  2. systemctl enable --now s3d"
	@echo "  3. journalctl -fu s3d"
	@echo "  4. s3dctl check"

## uninstall: remove binaries and systemd unit (does not remove config or logs)
uninstall:
	systemctl disable --now s3d 2>/dev/null || true
	rm -f $(INSTALL_DIR)/$(BINARY_AGENT) $(INSTALL_DIR)/$(BINARY_CTL)
	rm -f $(SERVICE_DIR)/s3d.service
	systemctl daemon-reload
	@echo "Uninstall complete. Config and logs preserved."

## package-deb: build a .deb package for distribution
package-deb: build-prod
	@echo "Packaging .deb ($(VERSION))..."
	@mkdir -p dist/deb/DEBIAN
	@mkdir -p dist/deb/usr/local/bin
	@mkdir -p dist/deb/etc/systemd/system
	@mkdir -p dist/deb/etc/plusclouds
	@mkdir -p dist/deb/var/log/plusclouds
	cp $(BUILD_DIR)/$(BINARY_AGENT) dist/deb/usr/local/bin/
	cp $(BUILD_DIR)/$(BINARY_CTL)   dist/deb/usr/local/bin/
	cp systemd/plusclouds-agent.service dist/deb/etc/systemd/system/
	cp configs/agent.yaml dist/deb/etc/plusclouds/agent.yaml
	@printf "Package: plusclouds-agent\n\
Version: $(VERSION)\n\
Section: utils\n\
Priority: optional\n\
Architecture: amd64\n\
Maintainer: PlusClouds <support@plusclouds.com>\n\
Description: PlusClouds Ubuntu VM Agent\n\
 A daemon for managing PlusClouds VMs via NATS.\n" > dist/deb/DEBIAN/control
	@printf "#!/bin/sh\n\
set -e\n\
mkdir -p /var/log/plusclouds\n\
chmod 0750 /var/log/plusclouds\n\
systemctl daemon-reload\n\
systemctl enable plusclouds-agent\n" > dist/deb/DEBIAN/postinst
	@chmod 0755 dist/deb/DEBIAN/postinst
	@printf "#!/bin/sh\n\
set -e\n\
systemctl disable --now plusclouds-agent 2>/dev/null || true\n" > dist/deb/DEBIAN/prerm
	@chmod 0755 dist/deb/DEBIAN/prerm
	dpkg-deb --build dist/deb dist/plusclouds-agent_$(VERSION)_amd64.deb
	@echo "Package built: dist/plusclouds-agent_$(VERSION)_amd64.deb"

## help: display this help
help:
	@sed -n 's/^## //p' $(MAKEFILE_LIST) | column -t -s ':' | sed -e 's/^/ /'
