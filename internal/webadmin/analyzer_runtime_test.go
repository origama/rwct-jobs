package webadmin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestHandleAnalyzerRuntimeReturnsConfiguredFields(t *testing.T) {
	t.Setenv("LLM_MODEL", "Qwen3.5-0.8B-Q4_K_M.gguf")
	t.Setenv("LLM_ENDPOINT", "http://llama-cpp:8080")
	t.Setenv("LLM_TIMEOUT", "90s")
	t.Setenv("LLM_MAX_TOKENS", "1024")
	t.Setenv("LLM_THINKING_ENABLED", "true")
	t.Setenv("LLM_MAX_CONCURRENCY", "4")
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
