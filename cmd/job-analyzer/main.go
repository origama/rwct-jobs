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

func getenvIntAlias(primary, legacy string, def int) int {
	if v := getenvInt(primary, def); v != def || os.Getenv(primary) != "" {
		return v
	}
	return getenvInt(legacy, def)
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

	cfg := analyzer.Config{
		DBPath:                 getenv("ITEM_DB_PATH", getenv("RSS_DB_PATH", "/data/rss-reader.db")),
		QueuePollInterval:      analyzer.MustEnvDuration("ANALYZER_QUEUE_POLL_INTERVAL", "1200ms"),
		LeaseDuration:          analyzer.MustEnvDuration("ANALYZER_QUEUE_LEASE_DURATION", "2m"),
		LLMEndpoint:            getenv("LLM_ENDPOINT", "http://llama-cpp:8080"),
		LLMModel:               getenv("LLM_MODEL", "qwen3.5-0.8b-instruct-q4_k_m.gguf"),
		LLMTimeout:             analyzer.MustEnvDuration("LLM_TIMEOUT", "20s"),
		LLMTimeoutMax:          analyzer.MustEnvDuration("LLM_TIMEOUT_MAX", "10m"),
		LLMTimeoutPer1KChars:   analyzer.MustEnvDuration("LLM_TIMEOUT_PER_1K_CHARS", "25s"),
		LLMTimeoutPer256Tokens: analyzer.MustEnvDuration("LLM_TIMEOUT_PER_256_TOKENS", "20s"),
		LLMMaxTokens:           getenvInt("LLM_MAX_TOKENS", 512),
		LLMThinking:            getenvBool("LLM_THINKING_ENABLED", false),
		LLMStrictJSON:          getenvBool("ANALYZER_LLM_STRICT_JSON", true),
		PromptTemplate:         getenv("ANALYZER_PROMPT_TEMPLATE", ""),
		CompactPromptTemplate:  getenv("ANALYZER_COMPACT_PROMPT_TEMPLATE", ""),
		SourceExtractor:        getenv("ANALYZER_SOURCE_EXTRACTOR", "basic"),
		SourceMinChars:         getenvInt("ANALYZER_SOURCE_MIN_CHARS_FOR_BASIC", 220),
		ScraplingEndpoint:      getenv("ANALYZER_SCRAPLING_ENDPOINT", "http://scrapling-sidecar:8088"),
		ScraplingTimeout:       analyzer.MustEnvDuration("ANALYZER_SCRAPLING_TIMEOUT", "8s"),
		ScraplingMaxChars:      getenvInt("ANALYZER_SCRAPLING_MAX_CHARS", 10000),
		MaxConcurrency:         getenvIntAlias("ANALYZER_MAX_PARALLEL_JOBS", "LLM_MAX_CONCURRENCY", 1),
		MaxJobsPerMin:          getenvInt("LLM_MAX_JOBS_PER_MIN", 20),
		MaxDeliveryAttempts:    getenvInt("ANALYZER_MAX_DELIVERY_ATTEMPTS", 3),
		RetryAttempts:          getenvInt("DLQ_RETRY_ATTEMPTS", 3),
		RetryBaseDelay:         analyzer.MustEnvDuration("DLQ_RETRY_BASE_DELAY", "2s"),
	}

	logger, shutdownTelemetry, err := telemetry.Bootstrap(ctx, telemetry.BootstrapConfig{
		Endpoint:       getenv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:    "job-analyzer",
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

	_ = health.StartServer(getenvInt("HEALTH_PORT", 8082))

	svc, err := analyzer.New(cfg)
	if err != nil {
		log.Fatal(err)
	}
	if err := svc.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
