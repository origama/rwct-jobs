package webadmin

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"rwct-agent/pkg/events"
)

func TestHandleItemRequeueClearsDispatchedAndEnqueues(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "webadmin.db")
	svc, err := New(Config{DBPath: dbPath, HTTPPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	itemID := "item-requeue-1"
	payload := `{"id":"item-requeue-1","role":"Go Engineer"}`
	now := time.Now().UTC()
	_, err = svc.db.Exec(
		`INSERT INTO rss_items(id,status,analyzed_payload_json,dispatched_at,updated_at)
		 VALUES(?,?,?,?,?)`,
		itemID, events.ItemStatusDispatched, payload, now, now,
	)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"id": itemID})
	req := httptest.NewRequest(http.MethodPost, "/api/db/items/requeue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleItemRequeue(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var status string
	var dispatchedAt sql.NullString
	err = svc.db.QueryRow(`SELECT status, dispatched_at FROM rss_items WHERE id = ?`, itemID).Scan(&status, &dispatchedAt)
	if err != nil {
		t.Fatal(err)
	}
	if status != events.ItemStatusAnalyzed {
		t.Fatalf("expected status ANALYZED, got %s", status)
	}
	if dispatchedAt.Valid && dispatchedAt.String != "" {
		t.Fatalf("expected dispatched_at NULL after requeue, got %q", dispatchedAt.String)
	}

	var qState, qPayload string
	err = svc.db.QueryRow(`SELECT state, payload_json FROM dispatch_queue WHERE item_id = ?`, itemID).Scan(&qState, &qPayload)
	if err != nil {
		t.Fatal(err)
	}
	if qState != "QUEUED" {
		t.Fatalf("expected dispatch_queue state QUEUED, got %s", qState)
	}
	if qPayload != payload {
		t.Fatalf("unexpected payload in queue: %s", qPayload)
	}
}

func TestHandleItemRequeueRejectsMissingAnalyzedPayload(t *testing.T) {
	t.Parallel()

	dbPath := filepath.Join(t.TempDir(), "webadmin.db")
	svc, err := New(Config{DBPath: dbPath, HTTPPort: 8090})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()

	itemID := "item-requeue-no-payload"
	_, err = svc.db.Exec(
		`INSERT INTO rss_items(id,status,updated_at) VALUES(?,?,?)`,
		itemID, events.ItemStatusAnalyzed, time.Now().UTC(),
	)
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]string{"id": itemID})
	req := httptest.NewRequest(http.MethodPost, "/api/db/items/requeue", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	svc.handleItemRequeue(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%s", rec.Code, rec.Body.String())
	}

	var count int
	err = svc.db.QueryRow(`SELECT COUNT(1) FROM dispatch_queue WHERE item_id = ?`, itemID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("expected no queued dispatch row, got %d", count)
	}
}
