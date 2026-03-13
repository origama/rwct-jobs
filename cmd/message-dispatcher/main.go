package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"rwct-agent/internal/dispatcher"
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

func main() {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))

	cfg := dispatcher.Config{
		DBPath:                 getenv("ITEM_DB_PATH", getenv("RSS_DB_PATH", "/data/rss-reader.db")),
		QueuePollInterval:      dispatcher.MustEnvDuration("DISPATCH_QUEUE_POLL_INTERVAL", "1200ms"),
		LeaseDuration:          dispatcher.MustEnvDuration("DISPATCH_QUEUE_LEASE_DURATION", "2m"),
		DestinationMode:        getenv("DESTINATION_MODE", "file"),
		FileSinkPath:           getenv("FILE_SINK_PATH", "/data/outbox/messages.md"),
		TemplatePath:           getenv("DISPATCH_TEMPLATE_FILE", "/app/configs/message.tmpl.md"),
		TelegramTemplatePath:   getenv("TELEGRAM_TEMPLATE_FILE", ""),
		RateLimitPerMin:        getenvInt("DISPATCH_RATE_LIMIT_PER_MIN", 30),
		RetryAttempts:          getenvInt("DLQ_RETRY_ATTEMPTS", 3),
		RetryBaseDelay:         dispatcher.MustEnvDuration("DLQ_RETRY_BASE_DELAY", "2s"),
		TelegramBotToken:       getenv("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:         getenv("TELEGRAM_CHAT_ID", ""),
		TelegramThreadID:       dispatcher.MustEnvInt("TELEGRAM_THREAD_ID", 0),
		TelegramParseMode:      getenv("TELEGRAM_PARSE_MODE", "Markdown"),
		TelegramDisablePreview: dispatcher.MustEnvBool("TELEGRAM_DISABLE_WEB_PAGE_PREVIEW", false),
		TelegramAPIBaseURL:     getenv("TELEGRAM_API_BASE_URL", "https://api.telegram.org"),
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
	_ = health.StartServer(getenvInt("HEALTH_PORT", 8083))

	svc, err := dispatcher.New(cfg)
	if err != nil {
		log.Fatal(err)
	}

	if err := svc.Run(ctx); err != nil && err != context.Canceled {
		log.Fatal(err)
	}
}
