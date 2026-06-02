//go:build linux

package main

import (
	"github.com/coreos/go-systemd/v22/dbus"
	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/modules/services"
)

// newServiceManager opens a system D-Bus connection for service health checks.
// Returns nil if D-Bus is unavailable (non-root, container, etc.) — the
// observer handles a nil manager gracefully, returning empty ServiceHealth fields.
func newServiceManager(logger *zap.Logger) services.Manager {
	conn, err := dbus.NewSystemdConnection()
	if err != nil {
		logger.Debug("systemd D-Bus unavailable; service health checks disabled", zap.Error(err))
		return services.New(nil, logger)
	}
	return services.New(conn, logger)
}
