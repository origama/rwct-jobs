package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"rwct-agent/internal/rssreader"
	"rwct-agent/pkg/health"
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
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))

	cfg := rssreader.Config{
		DBPath:                getenv("RSS_DB_PATH", "/data/rss-reader.db"),
		FeedsFile:             getenv("RSS_FEEDS_FILE", "/app/configs/feeds.txt"),
		FeedsCSV:              getenv("RSS_FEEDS", ""),
		BootstrapMarkExisting: getenvBool("RSS_BOOTSTRAP_MARK_EXISTING", false),
		ColdStartItemsPerFeed: getenvInt("RSS_COLD_START_ITEMS_PER_FEED", 3),
		PollInterval:          rssreader.MustEnvDuration("RSS_POLL_INTERVAL", "15m"),
		RetryAttempts:         getenvInt("RSS_RETRY_ATTEMPTS", 5),
		RetryBaseDelay:        rssreader.MustEnvDuration("RSS_RETRY_BASE_DELAY", "2s"),
		CooldownRetry:         rssreader.MustEnvDuration("RSS_COOLDOWN_RETRY", "6h"),
		HealthPort:            getenvInt("HEALTH_PORT", 8081),
		CleanupEvery:          rssreader.MustEnvDuration("RETENTION_CLEANUP_EVERY", "1h"),
		RetentionHours:        getenvInt("RETENTION_HOURS", 168),
		MaxItemsPerPoll:       getenvInt("RSS_MAX_ITEMS_PER_POLL", 100),
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if endpoint := getenv("OTEL_EXPORTER_OTLP_ENDPOINT", ""); endpoint != "" {
		shutdown, err := telemetry.InitMetrics(ctx, endpoint)
		if err != nil {
			slog.Error("otel metrics init failed", "err", err)
		} else {
			defer func() { _ = shutdown(context.Background()) }()
		}
	}

	_ = health.StartServer(cfg.HealthPort)

	svc, err := rssreader.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer svc.Close()

	if err := svc.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
