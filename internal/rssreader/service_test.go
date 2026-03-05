package rssreader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeTitle(t *testing.T) {
	in := "  Senior   Go Developer   REMOTE "
	got := NormalizeTitle(in)
	want := "senior go developer remote"
	if got != want {
		t.Fatalf("normalize title mismatch: got=%q want=%q", got, want)
	}
}

func TestNormalizeURL(t *testing.T) {
	raw := "https://example.com/jobs/1#section"
	got := normalizeURL(raw)
	want := "https://example.com/jobs/1"
	if got != want {
		t.Fatalf("normalize url mismatch: got=%q want=%q", got, want)
	}
}

func TestBootstrapMarkExisting(t *testing.T) {
	t.Parallel()
	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><item><guid>1</guid><title>Go Engineer</title><link>https://example.com/job/1</link><description>Remote role</description></item></channel></rss>`))
	}))
	defer feedServer.Close()

	db := filepath.Join(t.TempDir(), "rss.db")
	svc, err := New(Config{
		DBPath:                db,
		BootstrapMarkExisting: true,
		RetryAttempts:         1,
		RetryBaseDelay:        100 * time.Millisecond,
		MaxItemsPerPoll:       10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	_, marked, err := svc.bootstrapMarkExisting(context.Background(), []string{feedServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	if marked != 1 {
		t.Fatalf("expected 1 marked item, got %d", marked)
	}

	dup, err := svc.isDuplicate("1", "https://example.com/job/1", NormalizeTitle("Go Engineer"))
	if err != nil {
		t.Fatal(err)
	}
	if !dup {
		t.Fatalf("expected item to be marked as processed")
	}
}
