package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"rwct-agent/internal/webadmin"
	"rwct-agent/pkg/telemetry"
)

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return def
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger, shutdownTelemetry, err := telemetry.Bootstrap(ctx, telemetry.BootstrapConfig{
		Endpoint:       getenv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:    "web-admin",
		ServiceVersion: getenv("SERVICE_VERSION", "dev"),
		Environment:    getenv("DEPLOY_ENV", "local"),
		Level:          slog.LevelInfo,
		Insecure:       getenvBool("OTEL_EXPORTER_OTLP_INSECURE", true),
	})
	slog.SetDefault(logger)
	if err != nil {
		slog.Error("otel init failed", "err", err)
	}
	defer func() { _ = shutdownTelemetry(context.Background()) }()

	cfg := webadmin.Config{
		DBPath:         getenv("RSS_DB_PATH", "/data/rss-reader.db"),
		HTTPPort:       getenvInt("WEB_ADMIN_PORT", 8090),
		RetentionHours: getenvInt("RETENTION_HOURS", 168),
	}
	svc, err := webadmin.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer svc.Close()

	if err := svc.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
