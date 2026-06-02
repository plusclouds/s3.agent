//go:build windows

package main

import (
	"go.uber.org/zap"

	"github.com/plusclouds/ubuntu-agent/internal/modules/services"
)

func newServiceManager(logger *zap.Logger) services.Manager {
	return services.New(logger)
}
