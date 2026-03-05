package webadmin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestHandleFeedPollQueuesRequest(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "webadmin.db")
	svc, err := New(Config{DBPath: dbPath, HTTPPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	feedURL := "https://example.com/jobs.rss"
	_, err = svc.db.Exec(
		`INSERT INTO rss_feeds(feed_url, enabled, created_at, updated_at) VALUES(?,1,?,?)`,
		feedURL, time.Now().UTC(), time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"url": feedURL})
	req := httptest.NewRequest(http.MethodPost, "/api/db/feeds/poll", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleFeedPoll(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var count int
	if err := svc.db.QueryRow(`SELECT COUNT(1) FROM feed_poll_requests WHERE feed_url = ?`, feedURL).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one poll request, got %d", count)
	}
}
