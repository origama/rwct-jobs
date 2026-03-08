package analyzer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rwct-agent/pkg/events"
)

func TestNormalizeJobCategory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "exact", in: "Programmazione", want: "Programmazione"},
		{name: "case insensitive", in: "marketing", want: "Marketing"},
		{name: "alias devops", in: "devops/sysadmin", want: "Devops & Sysadmin"},
		{name: "english fallback alias", in: "other roles", want: "Altri ruoli"},
		{name: "unknown defaults", in: "data science", want: "Altri ruoli"},
		{name: "empty defaults", in: "", want: "Altri ruoli"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeJobCategory(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeJobCategory(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCallLLMIncludesEnableThinkingWhenEnabled(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"role\":\"Software Engineer\"}"}}]}`))
	}))
	defer srv.Close()

	svc := &Service{
		cfg:  Config{LLMEndpoint: srv.URL, LLMModel: "test-model", LLMThinking: true},
		http: &http.Client{Timeout: time.Second},
	}

	if _, err := svc.callLLM(context.Background(), "prompt", 123); err != nil {
		t.Fatalf("callLLM returned error: %v", err)
	}

	if got["enable_thinking"] != true {
		t.Fatalf("expected enable_thinking=true in request, got %#v", got["enable_thinking"])
	}
}

func TestCallLLMDoesNotIncludeEnableThinkingWhenDisabled(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"role\":\"Software Engineer\"}"}}]}`))
	}))
	defer srv.Close()

	svc := &Service{
		cfg:  Config{LLMEndpoint: srv.URL, LLMModel: "test-model", LLMThinking: false},
		http: &http.Client{Timeout: time.Second},
	}

	if _, err := svc.callLLM(context.Background(), "prompt", 123); err != nil {
		t.Fatalf("callLLM returned error: %v", err)
	}

	if _, exists := got["enable_thinking"]; exists {
		t.Fatalf("expected enable_thinking to be omitted, got %#v", got["enable_thinking"])
	}
}

func TestAnalyzeIncludesSourcePageBodyInPrompt(t *testing.T) {
	t.Parallel()

	sourcePage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><body><h1>Software Engineer</h1><p>Compensation EUR 80k-100k</p><p>Contract: Full-time</p><p>Stack: Go, Kubernetes</p></body></html>`))
	}))
	defer sourcePage.Close()

	var got map[string]any
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode llm request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"job_category\":\"Programmazione\",\"role\":\"Software Engineer\",\"company\":\"ACME\",\"seniority\":\"\",\"location\":\"\",\"remote_type\":\"\",\"tech_stack\":[\"go\",\"kubernetes\"],\"contract_type\":\"full-time\",\"salary\":\"EUR 80k-100k\",\"language\":\"it\",\"summary_it\":\"Ruolo software engineer in ACME.\",\"confidence\":0.8}"}}]}`))
	}))
	defer llm.Close()

	svc := &Service{
		cfg: Config{
			LLMEndpoint:      llm.URL,
			LLMModel:         "test-model",
			LLMTimeout:       5 * time.Second,
			LLMMaxTokens:     256,
			ScrapeSourcePage: true,
		},
		http: &http.Client{Timeout: 5 * time.Second},
	}

	in := events.RawJobItem{
		ID:          "scrape-prompt-test",
		URL:         sourcePage.URL,
		Title:       "Software Engineer",
		Description: "Annuncio feed sintetico senza salary.",
	}
	if _, err := svc.analyze(context.Background(), in); err != nil {
		t.Fatalf("analyze failed: %v", err)
	}

	msgs, ok := got["messages"].([]any)
	if !ok || len(msgs) < 2 {
		t.Fatalf("expected messages in llm request, got %#v", got["messages"])
	}
	second, ok := msgs[1].(map[string]any)
	if !ok {
		t.Fatalf("unexpected second message shape: %#v", msgs[1])
	}
	content := strings.TrimSpace(second["content"].(string))
	if !strings.Contains(content, "Compensation EUR 80k-100k") {
		t.Fatalf("expected prompt to include scraped page body, got: %s", content)
	}
}

func TestParseAnalyzedJobComputesQualityRankingWithoutHallucinatingMissingFields(t *testing.T) {
	t.Parallel()

	in := events.RawJobItem{
		ID:          "quality-test-1",
		URL:         "https://example.com/jobs/quality-test-1",
		Title:       "Backend Engineer",
		Description: "Ruolo ambiguo senza dettagli completi.",
	}

	content := `{
		"job_category":"Programmazione",
		"role":"Backend Engineer",
		"company":"ACME",
		"seniority":"n/a",
		"location":"Remote EU",
		"remote_type":"remote",
		"tech_stack":["Go","Kubernetes"],
		"contract_type":"full-time",
		"salary":"competitive",
		"language":"it",
		"summary_it":"Ruolo backend remoto con stack Go/Kubernetes.",
		"confidence":0.81
	}`

	out, err := parseAnalyzedJob(content, in)
	if err != nil {
		t.Fatalf("parseAnalyzedJob returned error: %v", err)
	}

	if out.Seniority != "" {
		t.Fatalf("expected seniority to be sanitized to empty, got %q", out.Seniority)
	}
	if out.Salary != "competitive" {
		t.Fatalf("expected salary value to be preserved, got %q", out.Salary)
	}
	if out.JobPostQualityScore != 60 {
		t.Fatalf("expected quality score 60, got %d", out.JobPostQualityScore)
	}
	if out.JobPostQualityRank != "C" {
		t.Fatalf("expected quality rank C, got %q", out.JobPostQualityRank)
	}
	if len(out.JobPostMissing) != 2 {
		t.Fatalf("expected 2 missing fields, got %#v", out.JobPostMissing)
	}
	if out.JobPostMissing[0] != "seniority" || out.JobPostMissing[1] != "salary" {
		t.Fatalf("unexpected missing field list: %#v", out.JobPostMissing)
	}
}

func TestParseAnalyzedJobMarksAllKeyFieldsMissingWhenAdLacksDetails(t *testing.T) {
	t.Parallel()

	in := events.RawJobItem{
		ID:          "quality-test-all-missing",
		URL:         "https://example.com/jobs/quality-test-all-missing",
		Title:       "Software Engineer",
		Description: "Annuncio generico senza dettagli su seniority, location, contratto, salary o stack.",
	}

	content := `{
		"job_category":"Programmazione",
		"role":"Software Engineer",
		"company":"ACME",
		"seniority":"n/a",
		"location":"non indicato",
		"remote_type":"",
		"tech_stack":["n/a"],
		"contract_type":"unknown",
		"salary":"tbd",
		"language":"it",
		"summary_it":"Annuncio sintetico senza dettagli tecnici o contrattuali.",
		"confidence":0.55
	}`

	out, err := parseAnalyzedJob(content, in)
	if err != nil {
		t.Fatalf("parseAnalyzedJob returned error: %v", err)
	}

	if out.Seniority != "" || out.Location != "" || out.ContractType != "" {
		t.Fatalf("expected unknown placeholders sanitized to empty, got seniority=%q location=%q contract=%q", out.Seniority, out.Location, out.ContractType)
	}
	if len(out.TechStack) != 0 {
		t.Fatalf("expected empty tech_stack after sanitization, got %#v", out.TechStack)
	}
	if out.JobPostQualityScore != 0 {
		t.Fatalf("expected quality score 0, got %d", out.JobPostQualityScore)
	}
	if out.JobPostQualityRank != "D" {
		t.Fatalf("expected quality rank D, got %q", out.JobPostQualityRank)
	}
	if len(out.JobPostMissing) != 5 {
		t.Fatalf("expected 5 missing fields, got %#v", out.JobPostMissing)
	}
}

func TestShouldQuarantineClaimForNonRetryableError(t *testing.T) {
	t.Parallel()

	svc, err := New(Config{
		DBPath:              filepath.Join(t.TempDir(), "analyzer.db"),
		QueuePollInterval:   200 * time.Millisecond,
		LeaseDuration:       2 * time.Minute,
		LLMEndpoint:         "http://127.0.0.1:9999",
		LLMModel:            "test",
		LLMTimeout:          2 * time.Second,
		LLMMaxTokens:        128,
		MaxConcurrency:      1,
		MaxJobsPerMin:       10,
		MaxDeliveryAttempts: 3,
		RetryAttempts:       1,
		RetryBaseDelay:      10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.db.Close()

	if !svc.shouldQuarantineClaim("x", errors.New("missing required analyzed fields")) {
		t.Fatalf("expected non-retryable error to force quarantine")
	}
}

func TestShouldQuarantineClaimWhenDeliveryAttemptsExceeded(t *testing.T) {
	t.Parallel()

	svc, err := New(Config{
		DBPath:              filepath.Join(t.TempDir(), "analyzer.db"),
		QueuePollInterval:   200 * time.Millisecond,
		LeaseDuration:       2 * time.Minute,
		LLMEndpoint:         "http://127.0.0.1:9999",
		LLMModel:            "test",
		LLMTimeout:          2 * time.Second,
		LLMMaxTokens:        128,
		MaxConcurrency:      1,
		MaxJobsPerMin:       10,
		MaxDeliveryAttempts: 3,
		RetryAttempts:       1,
		RetryBaseDelay:      10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	defer svc.db.Close()

	now := time.Now().UTC()
	_, err = svc.db.Exec(`INSERT INTO analyzer_queue(item_id,payload_json,state,enqueued_at,lease_owner,lease_until,delivery_count,updated_at)
VALUES(?, '{}', 'LEASED', ?, ?, ?, ?, ?)`,
		"item-1", now, svc.workerID, now.Add(2*time.Minute), 3, now,
	)
	if err != nil {
		t.Fatalf("insert analyzer_queue row: %v", err)
	}

	if !svc.shouldQuarantineClaim("item-1", errors.New("llm status: 503")) {
		t.Fatalf("expected quarantine when delivery_count exceeds threshold")
	}
}
