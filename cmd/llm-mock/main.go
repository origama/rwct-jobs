package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"rwct-agent/pkg/telemetry"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	logger, shutdownTelemetry, err := telemetry.Bootstrap(ctx, telemetry.BootstrapConfig{
		Endpoint:       getenv("OTEL_EXPORTER_OTLP_ENDPOINT", ""),
		ServiceName:    "llm-mock",
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

	h := http.NewServeMux()
	h.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	h.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, _ *http.Request) {
		resp := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]string{
					"content": `{"job_category":"Programmazione","role":"Software Engineer","company":"Unknown","seniority":"mid","location":"remote","remote_type":"remote","tech_stack":["go"],"tags":["go","remote"],"contract_type":"","salary":"","language":"it","summary_it":"Annuncio processato dal mock LLM.","confidence":0.5}`,
				},
			}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	srv := &http.Server{
		Addr:    ":8080",
		Handler: telemetry.WrapHTTPHandler(h, "llm-mock-http"),
	}
	slog.Info("llm-mock started", "addr", srv.Addr)
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		slog.Error("llm-mock server failed", "err", err)
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
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
