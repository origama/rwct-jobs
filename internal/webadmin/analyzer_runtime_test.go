package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestHandleAnalyzerRuntimeReturnsConfiguredFields(t *testing.T) {
	t.Setenv("LLM_MODEL", "Qwen3.5-0.8B-Q4_K_M.gguf")
	t.Setenv("LLM_ENDPOINT", "http://llama-cpp:8080")
	t.Setenv("LLM_TIMEOUT", "90s")
	t.Setenv("LLM_MAX_TOKENS", "1024")
	t.Setenv("LLM_THINKING_ENABLED", "true")
	t.Setenv("ANALYZER_MAX_PARALLEL_JOBS", "4")
	t.Setenv("LLM_MAX_JOBS_PER_MIN", "60")
	t.Setenv("ANALYZER_QUEUE_POLL_INTERVAL", "1500ms")
	t.Setenv("ANALYZER_QUEUE_LEASE_DURATION", "3m")

	svc, err := New(Config{DBPath: filepath.Join(t.TempDir(), "webadmin.db"), HTTPPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/analyzers", nil)
	rec := httptest.NewRecorder()
	svc.handleAnalyzerRuntime(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Analyzers []struct {
			Model             string `json:"model"`
			Endpoint          string `json:"endpoint"`
			Timeout           string `json:"timeout"`
			MaxTokens         int    `json:"max_tokens"`
			ThinkingEnabled   bool   `json:"thinking_enabled"`
			MaxConcurrency    int    `json:"max_concurrency"`
			MaxJobsPerMin     int    `json:"max_jobs_per_min"`
			QueuePollInterval string `json:"queue_poll_interval"`
			LeaseDuration     string `json:"lease_duration"`
		} `json:"analyzers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Analyzers) != 1 {
		t.Fatalf("expected 1 analyzer row, got %d", len(resp.Analyzers))
	}
	row := resp.Analyzers[0]
	if row.Model != "Qwen3.5-0.8B-Q4_K_M.gguf" {
		t.Fatalf("unexpected model: %s", row.Model)
	}
	if row.Endpoint != "http://llama-cpp:8080" {
		t.Fatalf("unexpected endpoint: %s", row.Endpoint)
	}
	if row.Timeout != "90s" || row.MaxTokens != 1024 || !row.ThinkingEnabled {
		t.Fatalf("unexpected runtime payload: %+v", row)
	}
	if row.MaxConcurrency != 4 || row.MaxJobsPerMin != 60 {
		t.Fatalf("unexpected concurrency/rate config: %+v", row)
	}
	if row.QueuePollInterval != "1500ms" || row.LeaseDuration != "3m" {
		t.Fatalf("unexpected queue timing config: %+v", row)
	}
}

func TestHandleAnalyzerRuntimePrefersLatestRuntimeSnapshot(t *testing.T) {
	t.Setenv("LLM_MODEL", "Qwen3.5-0.8B-Q4_K_M.gguf")

	svc, err := New(Config{DBPath: filepath.Join(t.TempDir(), "webadmin.db"), HTTPPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	_, err = svc.db.Exec(`INSERT INTO analyzer_runtime(
worker_id, name, model, endpoint, timeout, max_tokens, thinking_enabled, max_concurrency, max_jobs_per_min, queue_poll_interval, lease_duration, updated_at
) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"worker-1",
		"job-analyzer",
		"Qwen3.5-4B-Q4_K_M.gguf",
		"http://127.0.0.1:8080",
		"90s",
		1024,
		1,
		1,
		20,
		"1200ms",
		"2m",
		time.Now().UTC(),
	)
	if err != nil {
		t.Fatalf("insert analyzer_runtime: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/runtime/analyzers", nil)
	rec := httptest.NewRecorder()
	svc.handleAnalyzerRuntime(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Analyzers []struct {
			Model string `json:"model"`
		} `json:"analyzers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Analyzers) == 0 {
		t.Fatalf("expected analyzer rows, got empty payload")
	}
	if resp.Analyzers[0].Model != "Qwen3.5-4B-Q4_K_M.gguf" {
		t.Fatalf("expected runtime model from snapshot, got %q", resp.Analyzers[0].Model)
	}
}
