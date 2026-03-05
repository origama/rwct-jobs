package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"rwct-agent/internal/analyzer"
	"rwct-agent/internal/dispatcher"
	"rwct-agent/internal/rssreader"
)

func TestPipelineFromRSSToFileSink(t *testing.T) {
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><item><guid>1</guid><title>Go Engineer</title><link>https://example.com/job/1</link><description>Remote role at ACME</description></item></channel></rss>`))
	}))
	defer feedServer.Close()

	llmServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"job_category\":\"Programmazione\",\"role\":\"Go Engineer\",\"company\":\"ACME\",\"seniority\":\"mid\",\"location\":\"EU\",\"remote_type\":\"remote\",\"tech_stack\":[\"go\",\"mqtt\"],\"contract_type\":\"full-time\",\"salary\":\"\",\"language\":\"it\",\"summary_it\":\"Ruolo backend remoto\",\"confidence\":0.82}"}}]}`))
	}))
	defer llmServer.Close()

	tmp := t.TempDir()
	out := filepath.Join(tmp, "messages.md")
	db := filepath.Join(tmp, "rss.db")

	rss, err := rssreader.New(rssreader.Config{
		DBPath:                db,
		FeedsCSV:              feedServer.URL,
		BootstrapMarkExisting: false,
		PollInterval:          1 * time.Second,
		RetryAttempts:         1,
		RetryBaseDelay:        100 * time.Millisecond,
		CooldownRetry:         2 * time.Second,
		CleanupEvery:          30 * time.Second,
		RetentionHours:        168,
		MaxItemsPerPoll:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rss.Close()

	ana, err := analyzer.New(analyzer.Config{
		DBPath:            db,
		QueuePollInterval: 100 * time.Millisecond,
		LeaseDuration:     1 * time.Second,
		LLMEndpoint:       llmServer.URL,
		LLMModel:          "test",
		LLMTimeout:        5 * time.Second,
		LLMMaxTokens:      128,
		MaxConcurrency:    1,
		MaxJobsPerMin:     30,
		RetryAttempts:     1,
		RetryBaseDelay:    100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	dis, err := dispatcher.New(dispatcher.Config{
		DBPath:            db,
		QueuePollInterval: 100 * time.Millisecond,
		LeaseDuration:     1 * time.Second,
		DestinationMode:   "file",
		FileSinkPath:      out,
		RateLimitPerMin:   60,
		RetryAttempts:     1,
		RetryBaseDelay:    100 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go func() { _ = ana.Run(ctx) }()
	go func() { _ = dis.Run(ctx) }()
	go func() { _ = rss.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(out)
		if err == nil && strings.Contains(string(b), "ACME") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	b, _ := os.ReadFile(out)
	t.Fatalf("expected output file to contain analyzed message; got: %s", string(b))
}
