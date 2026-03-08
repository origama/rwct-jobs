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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
