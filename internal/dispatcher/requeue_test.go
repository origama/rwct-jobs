package dispatcher

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"rwct-agent/pkg/events"
)

func TestHandleClaimedMessageDuplicateCompletesClaim(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "dispatcher.db")
	svc, err := New(Config{
		DBPath:            dbPath,
		QueuePollInterval: 100 * time.Millisecond,
		LeaseDuration:     1 * time.Minute,
		DestinationMode:   "file",
		FileSinkPath:      filepath.Join(t.TempDir(), "out.md"),
		RateLimitPerMin:   60,
		RetryAttempts:     1,
		RetryBaseDelay:    50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.db.Close()

	itemID := "item-dup-1"
	now := time.Now().UTC()
	_, err = svc.db.Exec(
		`INSERT INTO rss_items(id,status,dispatched_at,updated_at) VALUES(?,?,?,?)`,
		itemID, events.ItemStatusDispatched, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	an := events.AnalyzedJob{ID: itemID, SourceURL: "https://example.com", Title: "T"}
	payload, _ := json.Marshal(an)
	_, err = svc.db.Exec(
		`INSERT INTO dispatch_queue(item_id,payload_json,state,enqueued_at,lease_owner,lease_until,updated_at)
		 VALUES(?,?,?,?,?,?,?)`,
		itemID, string(payload), "LEASED", now, svc.workerID, now.Add(1*time.Minute), now,
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.handleClaimedMessage(context.Background(), itemID, payload); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var state string
	var doneAt sql.NullString
	err = svc.db.QueryRow(`SELECT state, done_at FROM dispatch_queue WHERE item_id = ?`, itemID).Scan(&state, &doneAt)
	if err != nil {
		t.Fatal(err)
	}
	if state != "DONE" {
		t.Fatalf("expected state DONE, got %s", state)
	}
	if !doneAt.Valid || doneAt.String == "" {
		t.Fatalf("expected done_at to be set")
	}
}
