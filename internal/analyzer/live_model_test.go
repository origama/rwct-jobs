package analyzer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"rwct-agent/pkg/events"
)

func TestConfiguredLiveModelRespectsRequestParametersAndReturnsValidJSON(t *testing.T) {
	t.Parallel()

	settings := loadLiveModelSettings(t)
	if settings.endpoint == "" || settings.model == "" {
		t.Skip("live model settings not configured")
	}
	if os.Getenv("RUN_LIVE_MODEL_TESTS") == "" {
		t.Skip("RUN_LIVE_MODEL_TESTS not set; skipping live model test")
	}

	healthCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := assertEndpointReachable(healthCtx, settings.endpoint); err != nil {
		t.Skipf("live model endpoint not reachable from test process: %v", err)
	}

	var captured map[string]any
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read proxy request body: %v", err)
		}
		defer r.Body.Close()

		if err := json.Unmarshal(body, &captured); err != nil {
			t.Fatalf("decode proxy request json: %v", err)
		}

		targetURL := strings.TrimRight(settings.endpoint, "/") + "/v1/chat/completions"
		req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
		if err != nil {
			t.Fatalf("build proxy upstream request: %v", err)
		}
		req.Header = r.Header.Clone()

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("proxy upstream request failed: %v", err)
		}
		defer resp.Body.Close()

		for k, vals := range resp.Header {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			t.Fatalf("copy proxy upstream body: %v", err)
		}
	}))
	defer proxy.Close()

	svc := &Service{
		cfg: Config{
			LLMEndpoint:  proxy.URL,
			LLMModel:     settings.model,
			LLMTimeout:   settings.timeout,
			LLMMaxTokens: settings.maxTokens,
			LLMThinking:  settings.thinking,
		},
		http: &http.Client{Timeout: settings.timeout},
	}

	in := events.RawJobItem{
		SchemaVersion: events.SchemaVersion,
		ID:            "live-model-agentic-test",
		FeedURL:       "https://example.com/feed.rss",
		SourceLabel:   "example",
		GUID:          "live-model-agentic-test-guid",
		URL:           "https://example.com/jobs/live-model-agentic-test",
		Title:         "Senior DevOps / Platform Engineer (Go, Kubernetes) - Remote EU",
		Description: strings.Join([]string{
			"Acme Payments S.r.l. cerca un Senior DevOps / Platform Engineer per una piattaforma di pagamenti B2B.",
			"Il ruolo lavora su servizi Go esistenti e sull'affidabilita' della piattaforma Kubernetes.",
			"Remote-first, ma con disponibilita' a lavorare in fuso orario EU.",
			"Contratto full-time preferito; possibile anche collaborazione B2B per il candidato giusto.",
			"Stack esplicito: Go, Kubernetes, Terraform, PostgreSQL, CI/CD.",
			"Salario non indicato nell'annuncio.",
			"Fornisci un JSON valido e non inventare campi mancanti.",
		}, " "),
		Links:       []string{"https://example.com/jobs/live-model-agentic-test"},
		ImageURLs:   nil,
		PublishedAt: time.Now().UTC(),
		FetchedAt:   time.Now().UTC(),
	}

	ctx, cancelCall := context.WithTimeout(context.Background(), settings.timeout)
	defer cancelCall()

	content, err := svc.callLLM(ctx, buildPrompt(in, ""), settings.maxTokens)
	if err != nil {
		t.Fatalf("callLLM failed: %v", err)
	}

	assertCapturedModelRequest(t, captured, settings)

	var raw map[string]any
	if err := json.Unmarshal([]byte(content), &raw); err != nil {
		t.Fatalf("live model did not return valid json: %v; content=%s", err, content)
	}

	requiredKeys := []string{
		"job_category", "role", "company", "seniority", "location", "remote_type",
		"tech_stack", "contract_type", "salary", "language", "summary_it", "confidence",
	}
	for _, key := range requiredKeys {
		if _, ok := raw[key]; !ok {
			t.Fatalf("live model json missing key %q: %s", key, content)
		}
	}

	out, err := parseAnalyzedJob(content, in)
	if err != nil {
		t.Fatalf("parseAnalyzedJob failed: %v; content=%s", err, content)
	}

	if !containsFold(out.Company, "acme") {
		t.Fatalf("expected company to mention Acme, got %q", out.Company)
	}
	if !containsFold(out.Role, "devops") && !containsFold(out.Role, "platform") && !containsFold(out.Role, "sre") {
		t.Fatalf("expected role to reflect devops/platform nature, got %q", out.Role)
	}
	if !containsFold(out.Seniority, "senior") {
		t.Fatalf("expected seniority to mention senior, got %q", out.Seniority)
	}
	if !containsFold(out.Location, "eu") && !containsFold(out.Location, "europe") && !containsFold(out.Location, "remote") {
		t.Fatalf("expected location to mention EU/Europe/remote, got %q", out.Location)
	}
	if !containsFold(out.RemoteType, "remote") {
		t.Fatalf("expected remote_type to mention remote, got %q", out.RemoteType)
	}
	if !sliceContainsFold(out.TechStack, "go") || !sliceContainsFold(out.TechStack, "kubernetes") {
		t.Fatalf("expected tech_stack to include Go and Kubernetes, got %#v", out.TechStack)
	}
	if strings.TrimSpace(out.SummaryIT) == "" {
		t.Fatalf("expected non-empty summary_it, got empty")
	}
	if out.JobCategory != "Devops & Sysadmin" && out.JobCategory != "Programmazione" {
		t.Fatalf("expected job_category to be Devops & Sysadmin or Programmazione, got %q", out.JobCategory)
	}
	if out.Language == "" {
		t.Fatalf("expected language to be set")
	}
}

type liveModelSettings struct {
	endpoint  string
	model     string
	maxTokens int
	thinking  bool
	timeout   time.Duration
}

func loadLiveModelSettings(t *testing.T) liveModelSettings {
	t.Helper()

	fileValues := readDotEnv(t)
	settings := liveModelSettings{
		endpoint:  firstNonEmpty(os.Getenv("LLM_TEST_ENDPOINT"), os.Getenv("LLM_ENDPOINT"), fileValues["LLM_ENDPOINT"]),
		model:     firstNonEmpty(os.Getenv("LLM_MODEL"), fileValues["LLM_MODEL"]),
		maxTokens: mustAtoiDefault(firstNonEmpty(os.Getenv("LLM_MAX_TOKENS"), fileValues["LLM_MAX_TOKENS"]), 512),
		thinking:  mustBoolDefault(firstNonEmpty(os.Getenv("LLM_THINKING_ENABLED"), fileValues["LLM_THINKING_ENABLED"]), false),
		timeout:   mustDurationDefault(firstNonEmpty(os.Getenv("LLM_TIMEOUT"), fileValues["LLM_TIMEOUT"]), 90*time.Second),
	}
	return settings
}

func readDotEnv(t *testing.T) map[string]string {
	t.Helper()

	envPath := filepath.Clean(filepath.Join("..", "..", ".env"))
	b, err := os.ReadFile(envPath)
	if err != nil {
		return map[string]string{}
	}

	out := map[string]string{}
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out
}

func assertEndpointReachable(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode < 500 {
			return nil
		}
	}

	req2, err2 := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(endpoint, "/")+"/healthz", nil)
	if err2 != nil {
		return fmt.Errorf("health request failed: %w", err)
	}
	resp2, err2 := http.DefaultClient.Do(req2)
	if err2 != nil {
		if err != nil {
			return err
		}
		return err2
	}
	defer resp2.Body.Close()
	if resp2.StatusCode >= 500 {
		return fmt.Errorf("endpoint health returned status %d", resp2.StatusCode)
	}
	return nil
}

func assertCapturedModelRequest(t *testing.T, captured map[string]any, settings liveModelSettings) {
	t.Helper()

	if got := fmt.Sprint(captured["model"]); got != settings.model {
		t.Fatalf("expected model %q, got %q", settings.model, got)
	}
	if got := int(asFloat64(t, captured["max_tokens"], "max_tokens")); got != settings.maxTokens {
		t.Fatalf("expected max_tokens=%d, got %d", settings.maxTokens, got)
	}
	if got := asFloat64(t, captured["temperature"], "temperature"); got != 0.1 {
		t.Fatalf("expected temperature=0.1, got %v", got)
	}
	if settings.thinking {
		if got, ok := captured["enable_thinking"].(bool); !ok || !got {
			t.Fatalf("expected enable_thinking=true, got %#v", captured["enable_thinking"])
		}
	} else {
		if _, ok := captured["enable_thinking"]; ok {
			t.Fatalf("expected enable_thinking to be omitted, got %#v", captured["enable_thinking"])
		}
	}

	msgs, ok := captured["messages"].([]any)
	if !ok || len(msgs) < 2 {
		t.Fatalf("expected at least 2 messages in request, got %#v", captured["messages"])
	}
}

func asFloat64(t *testing.T, v any, name string) float64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("expected numeric %s, got %#v", name, v)
	}
	return f
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func sliceContainsFold(values []string, needle string) bool {
	for _, v := range values {
		if containsFold(v, needle) {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mustAtoiDefault(v string, def int) int {
	if strings.TrimSpace(v) == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func mustBoolDefault(v string, def bool) bool {
	switch strings.TrimSpace(strings.ToLower(v)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

func mustDurationDefault(v string, def time.Duration) time.Duration {
	if strings.TrimSpace(v) == "" {
		return def
	}
	d, err := time.ParseDuration(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return d
}
