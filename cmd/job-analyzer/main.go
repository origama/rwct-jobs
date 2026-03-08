package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"rwct-agent/internal/analyzer"
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

	cfg := analyzer.Config{
		DBPath:              getenv("ITEM_DB_PATH", getenv("RSS_DB_PATH", "/data/rss-reader.db")),
		QueuePollInterval:   analyzer.MustEnvDuration("ANALYZER_QUEUE_POLL_INTERVAL", "1200ms"),
		LeaseDuration:       analyzer.MustEnvDuration("ANALYZER_QUEUE_LEASE_DURATION", "2m"),
		LLMEndpoint:         getenv("LLM_ENDPOINT", "http://llama-cpp:8080"),
		LLMModel:            getenv("LLM_MODEL", "qwen3.5-0.8b-instruct-q4_k_m.gguf"),
		LLMTimeout:          analyzer.MustEnvDuration("LLM_TIMEOUT", "20s"),
		LLMMaxTokens:        getenvInt("LLM_MAX_TOKENS", 512),
		LLMThinking:         getenvBool("LLM_THINKING_ENABLED", false),
		ScrapeSourcePage:    getenvBool("ANALYZER_SCRAPE_SOURCE_PAGE", true),
		MaxConcurrency:      getenvInt("LLM_MAX_CONCURRENCY", 1),
		MaxJobsPerMin:       getenvInt("LLM_MAX_JOBS_PER_MIN", 20),
		MaxDeliveryAttempts: getenvInt("ANALYZER_MAX_DELIVERY_ATTEMPTS", 3),
		RetryAttempts:       getenvInt("DLQ_RETRY_ATTEMPTS", 3),
		RetryBaseDelay:      analyzer.MustEnvDuration("DLQ_RETRY_BASE_DELAY", "2s"),
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
	_ = health.StartServer(getenvInt("HEALTH_PORT", 8082))

	svc, err := analyzer.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := svc.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
