package rssreader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestProcessForcedPollsConsumesRequestAndPollsFeed(t *testing.T) {
	t.Parallel()

	feedServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<?xml version="1.0"?><rss version="2.0"><channel><title>test</title><item><guid>1</guid><title>Go Engineer</title><link>https://example.com/job/1</link><description>Remote role</description></item></channel></rss>`))
	}))
	defer feedServer.Close()

	dbPath := filepath.Join(t.TempDir(), "rss.db")
	svc, err := New(Config{
		DBPath:          dbPath,
		RetryAttempts:   1,
		RetryBaseDelay:  100 * time.Millisecond,
		CooldownRetry:   1 * time.Second,
		MaxItemsPerPoll: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	if err := svc.addFeed(feedServer.URL); err != nil {
		t.Fatal(err)
	}
	_, err = svc.db.Exec(`INSERT INTO feed_poll_requests(feed_url, requested_at) VALUES(?,?)`, feedServer.URL, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.processForcedPolls(context.Background()); err != nil {
		t.Fatal(err)
	}

	var reqCount int
	if err := svc.db.QueryRow(`SELECT COUNT(1) FROM feed_poll_requests WHERE feed_url = ?`, feedServer.URL).Scan(&reqCount); err != nil {
		t.Fatal(err)
	}
	if reqCount != 0 {
		t.Fatalf("expected request to be consumed, got %d", reqCount)
	}

	var itemCount int
	if err := svc.db.QueryRow(`SELECT COUNT(1) FROM rss_items WHERE source_feed_url = ?`, feedServer.URL).Scan(&itemCount); err != nil {
		t.Fatal(err)
	}
	if itemCount != 1 {
		t.Fatalf("expected one item after forced poll, got %d", itemCount)
	}
}
