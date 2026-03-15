package webadmin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	_ "modernc.org/sqlite"

	"rwct-agent/pkg/events"
	"rwct-agent/pkg/telemetry"
)

type Config struct {
	DBPath         string
	HTTPPort       int
	RetentionHours int
}

type Service struct {
	cfg  Config
	db   *sql.DB
	http *http.Server

	metricsReg           otelmetric.Registration
	queueItemsGauge      otelmetric.Int64ObservableGauge
	itemStatusGauge      otelmetric.Int64ObservableGauge
	itemQueueStateGauge  otelmetric.Int64ObservableGauge
}

type feedRow struct {
	URL           string     `json:"url"`
	Enabled       bool       `json:"enabled"`
	FailureCount  int        `json:"failure_count"`
	DownUntil     *time.Time `json:"down_until,omitempty"`
	UpdatedAt     *time.Time `json:"updated_at,omitempty"`
	LastRefreshAt *time.Time `json:"last_refresh_at,omitempty"`
	HealthStatus  string     `json:"health_status"`
	HealthReason  string     `json:"health_reason,omitempty"`
	ItemsCount    int64      `json:"items_count"`
	SourceLabel   string     `json:"source_label"`
}

type dbOverview struct {
	FeedsEnabled   int64 `json:"feeds_enabled"`
	FeedsDisabled  int64 `json:"feeds_disabled"`
	PendingItems   int64 `json:"pending_items"`
	ProcessedItems int64 `json:"processed_items"`
	AnalyzedItems  int64 `json:"analyzed_items"`
}

type analyzedItemRow struct {
	ID                 string    `json:"id"`
	ItemHash           string    `json:"item_hash"`
	CreatedAt          time.Time `json:"created_at"`
	Title              string    `json:"title"`
	URL                string    `json:"url"`
	Status             string    `json:"status"`
	QueueState         string    `json:"queue_state"`
	RawEnqueued        bool      `json:"raw_enqueued"`
	AnalyzedEnqueued   bool      `json:"analyzed_enqueued"`
	HasAnalyzedVersion bool      `json:"has_analyzed_version"`
}

type queueDepthRow struct {
	Name   string `json:"name"`
	Topic  string `json:"topic"`
	Queued int64  `json:"queued"`
}

type ratioPoint struct {
	Minute     string   `json:"minute"`
	NewCount   int64    `json:"new_count"`
	Dispatched int64    `json:"dispatched_count"`
	Ratio      *float64 `json:"ratio"`
}

type analyzerRuntimeRow struct {
	Name              string `json:"name"`
	Model             string `json:"model"`
	Endpoint          string `json:"endpoint"`
	Timeout           string `json:"timeout"`
	MaxTokens         int    `json:"max_tokens"`
	ThinkingEnabled   bool   `json:"thinking_enabled"`
	MaxConcurrency    int    `json:"max_concurrency"`
	MaxJobsPerMin     int    `json:"max_jobs_per_min"`
	QueuePollInterval string `json:"queue_poll_interval"`
	LeaseDuration     string `json:"lease_duration"`
}

var unsafeNameRe = regexp.MustCompile(`[^a-z0-9]+`)

var (
	rawQueueStates      = []string{"QUEUED", "LEASED", "DONE"}
	analyzedQueueStates = []string{"QUEUED", "LEASED", "DONE"}
	itemStatuses        = []string{
		events.ItemStatusNew,
		events.ItemStatusAnalyzed,
		events.ItemStatusDispatched,
		events.ItemStatusFailed,
	}
	itemQueueStates = []string{
		"not_enqueued_raw",
		"queued_raw",
		"inflight_raw",
		"processed_raw",
		"not_enqueued_analyzed",
		"queued_analyzed",
		"dispatched",
		"failed",
		"unknown",
	}
)

func New(cfg Config) (*Service, error) {
	if cfg.HTTPPort <= 0 {
		cfg.HTTPPort = 8090
	}
	db, err := sql.Open("sqlite", sqliteDSN(cfg.DBPath))
	if err != nil {
		return nil, err
	}
	if err := initDB(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Service{cfg: cfg, db: db}
	s.initMetrics()
	return s, nil
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func (s *Service) Close() error {
	if s.metricsReg != nil {
		_ = s.metricsReg.Unregister()
	}
	return s.db.Close()
}

func (s *Service) initMetrics() {
	meter := otel.Meter("rwct-agent/web-admin")
	s.queueItemsGauge, _ = meter.Int64ObservableGauge(
		"rwct_pipeline_queue_items",
		otelmetric.WithDescription("Current number of items in each queue state"),
	)
	s.itemStatusGauge, _ = meter.Int64ObservableGauge(
		"rwct_pipeline_items_by_status",
		otelmetric.WithDescription("Current number of items by business status"),
	)
	s.itemQueueStateGauge, _ = meter.Int64ObservableGauge(
		"rwct_pipeline_items_by_queue_state",
		otelmetric.WithDescription("Current number of items by derived queue state from FSM"),
	)
	reg, err := meter.RegisterCallback(s.observePipelineMetrics,
		s.queueItemsGauge,
		s.itemStatusGauge,
		s.itemQueueStateGauge,
	)
	if err != nil {
		slog.Warn("web-admin metrics callback registration failed", "err", err)
		return
	}
	s.metricsReg = reg
}

func (s *Service) observePipelineMetrics(ctx context.Context, observer otelmetric.Observer) error {
	// Queue FSM: analyzer_queue and dispatch_queue each expose QUEUED/LEASED/DONE.
	rawCounts := s.countQueueStates("analyzer_queue", rawQueueStates)
	for _, st := range rawQueueStates {
		observer.ObserveInt64(s.queueItemsGauge, rawCounts[st],
			otelmetric.WithAttributes(
				attribute.String("queue", "analyzer_queue"),
				attribute.String("pipeline_stage", "raw"),
				attribute.String("state", st),
			))
	}
	analyzedCounts := s.countQueueStates("dispatch_queue", analyzedQueueStates)
	for _, st := range analyzedQueueStates {
		observer.ObserveInt64(s.queueItemsGauge, analyzedCounts[st],
			otelmetric.WithAttributes(
				attribute.String("queue", "dispatch_queue"),
				attribute.String("pipeline_stage", "analyzed"),
				attribute.String("state", st),
			))
	}

	// Business FSM on rss_items.
	statusCounts := s.countItemStatuses(itemStatuses)
	for _, st := range itemStatuses {
		observer.ObserveInt64(s.itemStatusGauge, statusCounts[st],
			otelmetric.WithAttributes(attribute.String("status", st)))
	}

	// Derived queue-state view used by admin UI.
	queueStateCounts := s.countDerivedQueueStates(itemQueueStates)
	for _, st := range itemQueueStates {
		observer.ObserveInt64(s.itemQueueStateGauge, queueStateCounts[st],
			otelmetric.WithAttributes(attribute.String("queue_state", st)))
	}
	return nil
}

func (s *Service) countQueueStates(table string, expected []string) map[string]int64 {
	out := make(map[string]int64, len(expected))
	for _, st := range expected {
		out[st] = 0
	}
	q := fmt.Sprintf("SELECT state, COUNT(1) FROM %s GROUP BY state", table)
	rows, err := s.db.Query(q)
	if err != nil {
		slog.Warn("count queue states failed", "table", table, "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			slog.Warn("scan queue states failed", "table", table, "err", err)
			return out
		}
		out[st] = n
	}
	if err := rows.Err(); err != nil {
		slog.Warn("rows queue states failed", "table", table, "err", err)
	}
	return out
}

func (s *Service) countItemStatuses(expected []string) map[string]int64 {
	out := make(map[string]int64, len(expected))
	for _, st := range expected {
		out[st] = 0
	}
	rows, err := s.db.Query(`SELECT status, COUNT(1) FROM rss_items GROUP BY status`)
	if err != nil {
		slog.Warn("count item statuses failed", "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			slog.Warn("scan item statuses failed", "err", err)
			return out
		}
		out[st] = n
	}
	if err := rows.Err(); err != nil {
		slog.Warn("rows item statuses failed", "err", err)
	}
	return out
}

func (s *Service) countDerivedQueueStates(expected []string) map[string]int64 {
	out := make(map[string]int64, len(expected))
	for _, st := range expected {
		out[st] = 0
	}
	const q = `
SELECT queue_state, COUNT(1)
FROM (
	SELECT CASE
		WHEN i.status = 'NEW' AND aq.item_id IS NULL THEN 'not_enqueued_raw'
		WHEN i.status = 'NEW' AND aq.state = 'QUEUED' THEN 'queued_raw'
		WHEN i.status = 'NEW' AND aq.state = 'LEASED' THEN 'inflight_raw'
		WHEN i.status = 'NEW' AND aq.state = 'DONE' THEN 'processed_raw'
		WHEN i.status = 'ANALYZED' AND i.analyzed_enqueued_at IS NULL THEN 'not_enqueued_analyzed'
		WHEN i.status = 'ANALYZED' THEN 'queued_analyzed'
		WHEN i.status = 'DISPATCHED' THEN 'dispatched'
		WHEN i.status = 'FAILED' THEN 'failed'
		ELSE 'unknown'
	END AS queue_state
	FROM rss_items i
	LEFT JOIN analyzer_queue aq ON aq.item_id = i.id
) x
GROUP BY queue_state
`
	rows, err := s.db.Query(q)
	if err != nil {
		slog.Warn("count derived queue states failed", "err", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var st string
		var n int64
		if err := rows.Scan(&st, &n); err != nil {
			slog.Warn("scan derived queue states failed", "err", err)
			return out
		}
		out[st] = n
	}
	if err := rows.Err(); err != nil {
		slog.Warn("rows derived queue states failed", "err", err)
	}
	return out
}

func initDB(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS rss_feeds (
 feed_url TEXT PRIMARY KEY,
 enabled INTEGER NOT NULL DEFAULT 1,
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS feed_state (
 feed_url TEXT PRIMARY KEY,
 failure_count INTEGER NOT NULL DEFAULT 0,
 down_until DATETIME,
 updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS feed_poll_requests (
 feed_url TEXT PRIMARY KEY,
 requested_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS analyzed_items (
 id TEXT PRIMARY KEY,
 source_label TEXT,
 source_url TEXT,
 title TEXT,
 role TEXT,
 company TEXT,
 seniority TEXT,
 job_category TEXT,
 confidence REAL NOT NULL DEFAULT 0,
 created_at DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_analyzed_created_at ON analyzed_items(created_at);
CREATE INDEX IF NOT EXISTS idx_analyzed_job_category ON analyzed_items(job_category);
CREATE INDEX IF NOT EXISTS idx_analyzed_seniority ON analyzed_items(seniority);
CREATE INDEX IF NOT EXISTS idx_analyzed_source_label ON analyzed_items(source_label);

CREATE TABLE IF NOT EXISTS rss_items (
 id TEXT PRIMARY KEY,
 source_feed_url TEXT NOT NULL DEFAULT '',
 guid TEXT,
 url TEXT NOT NULL DEFAULT '',
 title TEXT NOT NULL DEFAULT '',
 title_norm TEXT NOT NULL DEFAULT '',
 item_hash TEXT,
 description TEXT,
 links_json TEXT NOT NULL DEFAULT '[]',
 image_urls_json TEXT NOT NULL DEFAULT '[]',
 published_at DATETIME,
 fetched_at DATETIME,
 raw_enqueued_at DATETIME,
 raw_enqueue_count INTEGER NOT NULL DEFAULT 0,
 analyzed_enqueued_at DATETIME,
 analyzed_enqueue_count INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'NEW',
 analyzed_at DATETIME,
 analyzed_payload_json TEXT,
 dispatched_at DATETIME,
 last_error TEXT,
 created_at DATETIME,
 updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_rss_items_item_hash ON rss_items(item_hash);
CREATE INDEX IF NOT EXISTS idx_rss_items_source_feed_url ON rss_items(source_feed_url);
CREATE INDEX IF NOT EXISTS idx_rss_items_status ON rss_items(status);

CREATE TABLE IF NOT EXISTS analyzer_queue (
 item_id TEXT PRIMARY KEY,
 payload_json TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'QUEUED',
 enqueued_at DATETIME NOT NULL,
 lease_owner TEXT,
 lease_until DATETIME,
 delivery_count INTEGER NOT NULL DEFAULT 0,
 done_at DATETIME,
 last_error TEXT,
 updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_analyzer_queue_state_enqueued ON analyzer_queue(state, enqueued_at);
CREATE INDEX IF NOT EXISTS idx_analyzer_queue_lease_until ON analyzer_queue(lease_until);

CREATE TABLE IF NOT EXISTS dispatch_queue (
 item_id TEXT PRIMARY KEY,
 payload_json TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'QUEUED',
 enqueued_at DATETIME NOT NULL,
 lease_owner TEXT,
 lease_until DATETIME,
 delivery_count INTEGER NOT NULL DEFAULT 0,
 done_at DATETIME,
 last_error TEXT,
 updated_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_dispatch_queue_state_enqueued ON dispatch_queue(state, enqueued_at);
CREATE INDEX IF NOT EXISTS idx_dispatch_queue_lease_until ON dispatch_queue(lease_until);

CREATE TABLE IF NOT EXISTS analyzer_runtime (
 worker_id TEXT PRIMARY KEY,
 name TEXT NOT NULL,
 model TEXT NOT NULL,
 endpoint TEXT NOT NULL,
 timeout TEXT NOT NULL,
 max_tokens INTEGER NOT NULL,
 thinking_enabled INTEGER NOT NULL,
 max_concurrency INTEGER NOT NULL,
 max_jobs_per_min INTEGER NOT NULL,
 queue_poll_interval TEXT NOT NULL,
 lease_duration TEXT NOT NULL,
 updated_at DATETIME NOT NULL
);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	return migrateRSSItemsColumns(db)
}

func migrateRSSItemsColumns(db *sql.DB) error {
	statements := []string{
		`ALTER TABLE rss_items ADD COLUMN source_feed_url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE rss_items ADD COLUMN guid TEXT`,
		`ALTER TABLE rss_items ADD COLUMN url TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE rss_items ADD COLUMN title TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE rss_items ADD COLUMN title_norm TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE rss_items ADD COLUMN item_hash TEXT`,
		`ALTER TABLE rss_items ADD COLUMN description TEXT`,
		`ALTER TABLE rss_items ADD COLUMN links_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE rss_items ADD COLUMN image_urls_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE rss_items ADD COLUMN published_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN fetched_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN raw_enqueued_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN raw_enqueue_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE rss_items ADD COLUMN analyzed_enqueued_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN analyzed_enqueue_count INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE rss_items ADD COLUMN status TEXT NOT NULL DEFAULT 'NEW'`,
		`ALTER TABLE rss_items ADD COLUMN analyzed_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN analyzed_payload_json TEXT`,
		`ALTER TABLE rss_items ADD COLUMN dispatched_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN last_error TEXT`,
		`ALTER TABLE rss_items ADD COLUMN created_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN updated_at DATETIME`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_rss_items_item_hash ON rss_items(item_hash)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_rss_items_source_feed_url ON rss_items(source_feed_url)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_rss_items_status ON rss_items(status)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS analyzer_queue (
 item_id TEXT PRIMARY KEY,
 payload_json TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'QUEUED',
 enqueued_at DATETIME NOT NULL,
 lease_owner TEXT,
 lease_until DATETIME,
 delivery_count INTEGER NOT NULL DEFAULT 0,
 done_at DATETIME,
 last_error TEXT,
 updated_at DATETIME NOT NULL
)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_analyzer_queue_state_enqueued ON analyzer_queue(state, enqueued_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_analyzer_queue_lease_until ON analyzer_queue(lease_until)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS dispatch_queue (
 item_id TEXT PRIMARY KEY,
 payload_json TEXT NOT NULL,
 state TEXT NOT NULL DEFAULT 'QUEUED',
 enqueued_at DATETIME NOT NULL,
 lease_owner TEXT,
 lease_until DATETIME,
 delivery_count INTEGER NOT NULL DEFAULT 0,
 done_at DATETIME,
 last_error TEXT,
 updated_at DATETIME NOT NULL
)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_dispatch_queue_state_enqueued ON dispatch_queue(state, enqueued_at)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_dispatch_queue_lease_until ON dispatch_queue(lease_until)`)
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS analyzer_runtime (
 worker_id TEXT PRIMARY KEY,
 name TEXT NOT NULL,
 model TEXT NOT NULL,
 endpoint TEXT NOT NULL,
 timeout TEXT NOT NULL,
 max_tokens INTEGER NOT NULL,
 thinking_enabled INTEGER NOT NULL,
 max_concurrency INTEGER NOT NULL,
 max_jobs_per_min INTEGER NOT NULL,
 queue_poll_interval TEXT NOT NULL,
 lease_duration TEXT NOT NULL,
 updated_at DATETIME NOT NULL
)`)
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/api/db/overview", s.handleOverview)
	mux.HandleFunc("/api/db/feeds", s.handleFeeds)
	mux.HandleFunc("/api/db/feeds/toggle", s.handleFeedToggle)
	mux.HandleFunc("/api/db/feeds/poll", s.handleFeedPoll)
	mux.HandleFunc("/api/db/feed-items", s.handleFeedItems)
	mux.HandleFunc("/api/db/items/requeue", s.handleItemRequeue)
	mux.HandleFunc("/api/db/items", s.handleItemDelete)
	mux.HandleFunc("/api/db/items/analyzed", s.handleItemAnalyzedVersion)
	mux.HandleFunc("/api/db/items/status", s.handleItemStatus)
	mux.HandleFunc("/api/monitor/queues", s.handleQueueMonitor)
	mux.HandleFunc("/api/runtime/analyzers", s.handleAnalyzerRuntime)

	s.http = &http.Server{
		Addr:    fmt.Sprintf(":%d", s.cfg.HTTPPort),
		Handler: telemetry.WrapHTTPHandler(mux, "web-admin-http"),
	}

	go func() {
		if err := s.http.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("web-admin http server failed", "err", err)
		}
	}()
	slog.Info("web-admin started", "port", s.cfg.HTTPPort)

	<-ctx.Done()
	_ = s.http.Shutdown(context.Background())
	return ctx.Err()
}

func (s *Service) storeAnalyzed(payload []byte) error {
	var in events.AnalyzedJob
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		return fmt.Errorf("missing id")
	}
	if in.JobCategory == "" {
		in.JobCategory = "Altri ruoli"
	}
	_, err := s.db.Exec(`
INSERT INTO analyzed_items(id, source_label, source_url, title, role, company, seniority, job_category, confidence, created_at)
VALUES(?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(id) DO UPDATE SET
 source_label=excluded.source_label,
 source_url=excluded.source_url,
 title=excluded.title,
 role=excluded.role,
 company=excluded.company,
 seniority=excluded.seniority,
 job_category=excluded.job_category,
 confidence=excluded.confidence,
 created_at=excluded.created_at
`,
		in.ID,
		in.SourceLabel,
		in.SourceURL,
		in.Title,
		in.Role,
		in.Company,
		in.Seniority,
		in.JobCategory,
		in.Confidence,
		in.CreatedAt.UTC(),
	)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(
		`UPDATE rss_items
SET status = ?, analyzed_at = ?, analyzed_payload_json = ?, analyzed_enqueued_at = COALESCE(?, analyzed_enqueued_at), last_error = '', updated_at = ?
WHERE id = ? AND status <> ?`,
		events.ItemStatusAnalyzed,
		time.Now().UTC(),
		string(payload),
		time.Now().UTC(),
		time.Now().UTC(),
		in.ID,
		events.ItemStatusDispatched,
	)
	return nil
}

func (s *Service) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Service) handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(indexHTML))
}

func (s *Service) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	res := dbOverview{
		FeedsEnabled:   queryCount(s.db, `SELECT COUNT(1) FROM rss_feeds WHERE enabled = 1`),
		FeedsDisabled:  queryCount(s.db, `SELECT COUNT(1) FROM rss_feeds WHERE enabled = 0`),
		PendingItems:   queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE status = ?`, events.ItemStatusNew),
		ProcessedItems: queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE status = ?`, events.ItemStatusDispatched),
		AnalyzedItems:  queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE status = ?`, events.ItemStatusAnalyzed),
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Service) handleFeeds(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.Query(`
SELECT f.feed_url, f.enabled, COALESCE(fs.failure_count, 0), fs.down_until, fs.updated_at
FROM rss_feeds f
LEFT JOIN feed_state fs ON fs.feed_url = f.feed_url
ORDER BY f.feed_url`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		now := time.Now().UTC()
		out := make([]feedRow, 0)
		for rows.Next() {
			var fr feedRow
			var enabledInt int
			var down sql.NullTime
			var upd sql.NullTime
			if err := rows.Scan(&fr.URL, &enabledInt, &fr.FailureCount, &down, &upd); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			fr.Enabled = enabledInt == 1
			if down.Valid {
				t := down.Time.UTC()
				fr.DownUntil = &t
			}
			if upd.Valid {
				t := upd.Time.UTC()
				fr.UpdatedAt = &t
				fr.LastRefreshAt = &t
			}
			fr.HealthStatus = "ok"
			if !fr.Enabled {
				fr.HealthStatus = "disabled"
				fr.HealthReason = "feed disabilitato"
			} else if down.Valid && down.Time.UTC().After(now) {
				fr.HealthStatus = "broken"
				fr.HealthReason = "feed in errore (cooldown attivo)"
			}
			fr.SourceLabel = sourceLabelFromFeedURL(fr.URL)
			fr.ItemsCount = queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE source_feed_url = ?`, fr.URL)
			out = append(out, fr)
		}
		if err := rows.Err(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	case http.MethodPost:
		var req struct {
			URL string `json:"url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
			return
		}
		feedURL := strings.TrimSpace(req.URL)
		if !isValidFeedURL(feedURL) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid url"})
			return
		}
		_, err := s.db.Exec(
			`INSERT INTO rss_feeds(feed_url, enabled, created_at, updated_at) VALUES(?,1,?,?)
ON CONFLICT(feed_url) DO UPDATE SET enabled=1, updated_at=excluded.updated_at`,
			feedURL,
			time.Now().UTC(),
			time.Now().UTC(),
		)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
	case http.MethodDelete:
		feedURL := strings.TrimSpace(r.URL.Query().Get("url"))
		if !isValidFeedURL(feedURL) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid url"})
			return
		}
		tx, err := s.db.Begin()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		defer func() { _ = tx.Rollback() }()
		res, err := tx.Exec(`DELETE FROM rss_feeds WHERE feed_url = ?`, feedURL)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if _, err := tx.Exec(`DELETE FROM feed_state WHERE feed_url = ?`, feedURL); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
			return
		}
		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleFeedToggle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL     string `json:"url"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	feedURL := strings.TrimSpace(req.URL)
	if !isValidFeedURL(feedURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid url"})
		return
	}
	value := 0
	if req.Enabled {
		value = 1
	}
	res, err := s.db.Exec(`UPDATE rss_feeds SET enabled=?, updated_at=? WHERE feed_url=?`, value, time.Now().UTC(), feedURL)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) handleFeedPoll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	feedURL := strings.TrimSpace(req.URL)
	if !isValidFeedURL(feedURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid url"})
		return
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM rss_feeds WHERE feed_url = ?`, feedURL).Scan(&exists); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if exists == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "feed not found"})
		return
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`INSERT INTO feed_poll_requests(feed_url, requested_at) VALUES(?,?)
ON CONFLICT(feed_url) DO UPDATE SET requested_at=excluded.requested_at`,
		feedURL, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "requested_at": now})
}

func (s *Service) handleFeedItems(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	feedURL := strings.TrimSpace(r.URL.Query().Get("feed_url"))
	if !isValidFeedURL(feedURL) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid feed_url"})
		return
	}
	limit := 100
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	rows, err := s.db.Query(`
SELECT i.id, i.item_hash, i.created_at, i.title, i.url, i.status,
       CASE
         WHEN i.status = 'NEW' AND aq.item_id IS NULL THEN 'not_enqueued_raw'
         WHEN i.status = 'NEW' AND aq.state = 'QUEUED' THEN 'queued_raw'
         WHEN i.status = 'NEW' AND aq.state = 'LEASED' THEN 'inflight_raw'
         WHEN i.status = 'NEW' AND aq.state = 'DONE' THEN 'processed_raw'
         WHEN i.status = 'ANALYZED' AND i.analyzed_enqueued_at IS NULL THEN 'not_enqueued_analyzed'
         WHEN i.status = 'ANALYZED' THEN 'queued_analyzed'
         WHEN i.status = 'DISPATCHED' THEN 'dispatched'
         WHEN i.status = 'FAILED' THEN 'failed'
         ELSE 'unknown'
       END AS queue_state,
       CASE WHEN aq.item_id IS NOT NULL OR i.status IN ('ANALYZED','DISPATCHED') THEN 1 ELSE 0 END AS raw_enqueued,
       CASE WHEN i.analyzed_enqueued_at IS NOT NULL OR i.status = 'DISPATCHED' THEN 1 ELSE 0 END AS analyzed_enqueued,
       CASE WHEN TRIM(COALESCE(i.analyzed_payload_json,'')) <> '' THEN 1 ELSE 0 END AS has_analyzed
FROM rss_items i
LEFT JOIN analyzer_queue aq ON aq.item_id = i.id
WHERE i.source_feed_url = ?
ORDER BY i.created_at DESC
LIMIT ?
`, feedURL, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	out := make([]analyzedItemRow, 0)
	for rows.Next() {
		var it analyzedItemRow
		var hasAnalyzed, rawEnqueued, analyzedEnqueued int
		if err := rows.Scan(
			&it.ID, &it.ItemHash, &it.CreatedAt, &it.Title, &it.URL, &it.Status,
			&it.QueueState, &rawEnqueued, &analyzedEnqueued, &hasAnalyzed,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		it.RawEnqueued = rawEnqueued == 1
		it.AnalyzedEnqueued = analyzedEnqueued == 1
		it.HasAnalyzedVersion = hasAnalyzed == 1
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"feed_url":     feedURL,
		"source_label": sourceLabelFromFeedURL(feedURL),
		"items":        out,
	})
}

func (s *Service) handleItemRequeue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	itemID := strings.TrimSpace(req.ID)
	if itemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}

	var analyzedJSON string
	err := s.db.QueryRow(
		`SELECT COALESCE(analyzed_payload_json,'')
FROM rss_items WHERE id = ?`,
		itemID,
	).Scan(&analyzedJSON)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	now := time.Now().UTC()
	if strings.TrimSpace(analyzedJSON) == "" {
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "missing analyzed payload: requeue richiede una processed version già disponibile",
		})
		return
	}

	_, err = s.db.Exec(
		`INSERT INTO dispatch_queue(item_id, payload_json, state, enqueued_at, updated_at)
VALUES(?, ?, 'QUEUED', ?, ?)
ON CONFLICT(item_id) DO UPDATE SET
 payload_json=excluded.payload_json,
 state='QUEUED',
 lease_owner=NULL,
 lease_until=NULL,
 last_error='',
 updated_at=excluded.updated_at`,
		itemID, analyzedJSON, now, now,
	)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, _ = s.db.Exec(
		`UPDATE rss_items
		 SET status=?,
		     analyzed_enqueued_at=COALESCE(?, analyzed_enqueued_at),
		     analyzed_enqueue_count=COALESCE(analyzed_enqueue_count,0)+1,
		     dispatched_at=NULL,
		     last_error='',
		     updated_at=?
		 WHERE id=?`,
		events.ItemStatusAnalyzed, now, now, itemID,
	)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "analyzed"})
}

func (s *Service) handleItemDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	itemID := strings.TrimSpace(r.URL.Query().Get("id"))
	if itemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	tx, err := s.db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`DELETE FROM rss_items WHERE id = ?`, itemID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	_, _ = tx.Exec(`DELETE FROM analyzed_items WHERE id = ?`, itemID)
	_, _ = tx.Exec(`DELETE FROM analyzer_queue WHERE item_id = ?`, itemID)
	_, _ = tx.Exec(`DELETE FROM dispatch_queue WHERE item_id = ?`, itemID)
	affected, _ := res.RowsAffected()
	if affected == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found"})
		return
	}
	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Service) handleItemAnalyzedVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	itemID := strings.TrimSpace(r.URL.Query().Get("id"))
	if itemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}
	var payload string
	err := s.db.QueryRow(`SELECT COALESCE(analyzed_payload_json,'') FROM rss_items WHERE id = ?`, itemID).Scan(&payload)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(payload) == "" {
		var b []byte
		err := s.db.QueryRow(`SELECT json_object(
			'schema_version', ?, 'id', id, 'source_label', source_label, 'source_url', source_url, 'title', title,
			'job_category', job_category, 'role', role, 'company', company, 'seniority', seniority, 'confidence', confidence, 'created_at', created_at
		) FROM analyzed_items WHERE id = ?`, events.SchemaVersion, itemID).Scan(&b)
		if err == nil {
			payload = string(b)
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": itemID, "analyzed_payload_json": payload})
}

func (s *Service) handleItemStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	itemID := strings.TrimSpace(r.URL.Query().Get("id"))
	if itemID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing id"})
		return
	}

	var status, lastError string
	var updatedAt time.Time
	var analyzedAt, dispatchedAt sql.NullTime
	err := s.db.QueryRow(
		`SELECT status, COALESCE(last_error,''), updated_at, analyzed_at, dispatched_at FROM rss_items WHERE id = ?`,
		itemID,
	).Scan(&status, &lastError, &updatedAt, &analyzedAt, &dispatchedAt)
	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "item not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	var analyzedAtPtr, dispatchedAtPtr *time.Time
	if analyzedAt.Valid {
		t := analyzedAt.Time.UTC()
		analyzedAtPtr = &t
	}
	if dispatchedAt.Valid {
		t := dispatchedAt.Time.UTC()
		dispatchedAtPtr = &t
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"id":            itemID,
		"status":        status,
		"last_error":    lastError,
		"updated_at":    updatedAt.UTC(),
		"analyzed_at":   analyzedAtPtr,
		"dispatched_at": dispatchedAtPtr,
	})
}

func (s *Service) handleQueueMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	rawQueued := queryCount(s.db, `SELECT COUNT(1) FROM analyzer_queue WHERE state = 'QUEUED'`)
	rawLeased := queryCount(s.db, `SELECT COUNT(1) FROM analyzer_queue WHERE state = 'LEASED'`)
	analyzedQueued := queryCount(s.db, `SELECT COUNT(1) FROM dispatch_queue WHERE state = 'QUEUED'`)
	analyzedLeased := queryCount(s.db, `SELECT COUNT(1) FROM dispatch_queue WHERE state = 'LEASED'`)
	failedQueued := queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE status = ?`, events.ItemStatusFailed)
	dispatchedTotal := queryCount(s.db, `SELECT COUNT(1) FROM rss_items WHERE status = ? AND dispatched_at IS NOT NULL`, events.ItemStatusDispatched)

	now := time.Now().UTC()
	stuckRaw10m := queryCount(s.db, `SELECT COUNT(1) FROM analyzer_queue WHERE state IN ('QUEUED','LEASED') AND enqueued_at <= ?`, now.Add(-10*time.Minute))
	stuckAnalyzed10m, _ := countStuckByStatus(s.db, events.ItemStatusAnalyzed, "analyzed_at", now.Add(-10*time.Minute))
	new5mMap, _ := countByMinute(s.db, "created_at", now.Add(-5*time.Minute))
	dispatched5mMap, _ := countByMinute(s.db, "dispatched_at", now.Add(-5*time.Minute))
	var new5m, dispatched5m int64
	for _, v := range new5mMap {
		new5m += v
	}
	for _, v := range dispatched5mMap {
		dispatched5m += v
	}

	points, err := s.last3HoursRatioPoints()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"queues": []queueDepthRow{
			{Name: "raw_backlog", Topic: "sqlite://analyzer_queue?state=QUEUED", Queued: rawQueued},
			{Name: "raw_inflight", Topic: "sqlite://analyzer_queue?state=LEASED", Queued: rawLeased},
			{Name: "analyzed_backlog", Topic: "sqlite://dispatch_queue?state=QUEUED", Queued: analyzedQueued},
			{Name: "analyzed_inflight", Topic: "sqlite://dispatch_queue?state=LEASED", Queued: analyzedLeased},
			{Name: "failed", Topic: "rss_items.status=FAILED", Queued: failedQueued},
		},
		"pipeline": map[string]any{
			"dispatched_total":          dispatchedTotal,
			"stuck_new_10m":             stuckRaw10m,
			"stuck_analyzed_10m":        stuckAnalyzed10m,
			"new_last_5m":               new5m,
			"dispatched_last_5m":        dispatched5m,
			"consumer_blocked_raw":      stuckRaw10m > 0 && rawQueued > 0,
			"consumer_blocked_analyzed": stuckAnalyzed10m > 0 && analyzedQueued > 0,
		},
		"ratio_window": map[string]any{
			"minutes": 180,
			"points":  points,
		},
	})
}

func (s *Service) handleAnalyzerRuntime(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	rows, err := s.db.Query(`
SELECT name, model, endpoint, timeout, max_tokens, thinking_enabled, max_concurrency, max_jobs_per_min, queue_poll_interval, lease_duration
FROM analyzer_runtime
WHERE updated_at >= ?
ORDER BY updated_at DESC
`, time.Now().UTC().Add(-20*time.Minute))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	out := make([]analyzerRuntimeRow, 0)
	for rows.Next() {
		var row analyzerRuntimeRow
		var thinking int
		if err := rows.Scan(
			&row.Name, &row.Model, &row.Endpoint, &row.Timeout, &row.MaxTokens, &thinking,
			&row.MaxConcurrency, &row.MaxJobsPerMin, &row.QueuePollInterval, &row.LeaseDuration,
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		row.ThinkingEnabled = thinking == 1
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(out) == 0 {
		out = append(out, analyzerRuntimeRow{
			Name:              "job-analyzer",
			Model:             getenv("LLM_MODEL", "qwen3.5-0.8b-instruct-q4_k_m.gguf"),
			Endpoint:          getenv("LLM_ENDPOINT", "http://llama-cpp:8080"),
			Timeout:           getenv("LLM_TIMEOUT", "20s"),
			MaxTokens:         getenvInt("LLM_MAX_TOKENS", 512),
			ThinkingEnabled:   getenvBool("LLM_THINKING_ENABLED", false),
			MaxConcurrency:    getenvIntAlias("ANALYZER_MAX_PARALLEL_JOBS", "LLM_MAX_CONCURRENCY", 1),
			MaxJobsPerMin:     getenvInt("LLM_MAX_JOBS_PER_MIN", 20),
			QueuePollInterval: getenv("ANALYZER_QUEUE_POLL_INTERVAL", "1200ms"),
			LeaseDuration:     getenv("ANALYZER_QUEUE_LEASE_DURATION", "2m"),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{"analyzers": out})
}

func (s *Service) last3HoursRatioPoints() ([]ratioPoint, error) {
	now := time.Now().UTC().Truncate(time.Minute)
	start := now.Add(-179 * time.Minute)

	newByMinute, err := countByMinute(s.db, "created_at", start)
	if err != nil {
		return nil, err
	}
	dispatchedByMinute, err := countByMinute(s.db, "dispatched_at", start)
	if err != nil {
		return nil, err
	}

	points := make([]ratioPoint, 0, 180)
	for i := 0; i < 180; i++ {
		t := start.Add(time.Duration(i) * time.Minute)
		key := t.Format(time.RFC3339)
		nc := newByMinute[key]
		dc := dispatchedByMinute[key]
		var ratio *float64
		if dc > 0 {
			r := float64(nc) / float64(dc)
			ratio = &r
		}
		points = append(points, ratioPoint{
			Minute:     key,
			NewCount:   nc,
			Dispatched: dc,
			Ratio:      ratio,
		})
	}
	return points, nil
}

func countByMinute(db *sql.DB, tsCol string, since time.Time) (map[string]int64, error) {
	if tsCol != "created_at" && tsCol != "dispatched_at" {
		return nil, fmt.Errorf("unsupported ts column: %s", tsCol)
	}
	query := fmt.Sprintf(`
SELECT %s
FROM rss_items
WHERE %s IS NOT NULL
`, tsCol, tsCol)
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if !raw.Valid {
			continue
		}
		t, ok := parseDBTime(raw.String)
		if !ok {
			continue
		}
		if t.Time.Before(since) {
			continue
		}
		minute := t.Time.UTC().Truncate(time.Minute).Format(time.RFC3339)
		out[minute]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func parseDBTime(v string) (sql.NullTime, bool) {
	s := strings.TrimSpace(v)
	if s == "" {
		return sql.NullTime{}, false
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05",
	}
	for _, layout := range layouts {
		if t, err := time.Parse(layout, s); err == nil {
			return sql.NullTime{Time: t, Valid: true}, true
		}
	}
	return sql.NullTime{}, false
}

func countStuckByStatus(db *sql.DB, status, tsCol string, cutoff time.Time) (int64, error) {
	if tsCol != "created_at" && tsCol != "analyzed_at" && tsCol != "updated_at" {
		return 0, fmt.Errorf("unsupported ts column: %s", tsCol)
	}
	query := fmt.Sprintf(`SELECT %s FROM rss_items WHERE status = ? AND %s IS NOT NULL`, tsCol, tsCol)
	rows, err := db.Query(query, status)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	var n int64
	for rows.Next() {
		var raw sql.NullString
		if err := rows.Scan(&raw); err != nil {
			return 0, err
		}
		if !raw.Valid {
			continue
		}
		t, ok := parseDBTime(raw.String)
		if !ok {
			continue
		}
		if t.Time.UTC().Before(cutoff) {
			n++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}

func sourceLabelFromFeedURL(feedURL string) string {
	u, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil {
		return "feed"
	}
	host := strings.TrimPrefix(strings.ToLower(u.Hostname()), "www.")
	host = strings.ReplaceAll(host, ".", "_")

	last := ""
	for _, p := range strings.Split(strings.Trim(u.Path, "/"), "/") {
		if strings.TrimSpace(p) != "" {
			last = p
		}
	}
	last = strings.ToLower(last)
	last = strings.TrimSuffix(last, ".rss")
	last = strings.TrimSuffix(last, ".xml")
	last = unsafeNameRe.ReplaceAllString(last, "_")
	last = strings.Trim(last, "_")

	if last == "" || last == "feed" {
		return host
	}
	return host + "_" + last
}

func queryCount(db *sql.DB, q string, args ...any) int64 {
	var n int64
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func getenvInt(k string, def int) int {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func getenvIntAlias(primary, legacy string, def int) int {
	if v := getenvInt(primary, def); v != def || strings.TrimSpace(os.Getenv(primary)) != "" {
		return v
	}
	return getenvInt(legacy, def)
}

func getenvBool(k string, def bool) bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv(k))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	case "":
		return def
	default:
		return def
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func isValidFeedURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

const indexHTML = `<!doctype html>
<html lang="it">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>RWCT DB Explorer</title>
<style>
:root { color-scheme: light; }
body { margin:0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background:#f5f7fb; color:#1f2937; }
header { padding:14px 18px; background:#0f172a; color:#e2e8f0; }
main { padding:16px; display:grid; grid-template-columns: 460px 1fr; gap:14px; }
.card { background:#fff; border:1px solid #dbe2ea; border-radius:10px; padding:12px; }
h1, h2, h3 { margin:0 0 10px; }
h1 { font-size:16px; }
h2 { font-size:14px; }
.small { font-size:12px; color:#64748b; }
.row { display:flex; align-items:center; gap:8px; flex-wrap:wrap; }
.pill { font-size:11px; padding:2px 8px; border-radius:999px; border:1px solid #cbd5e1; background:#f8fafc; }
.pill.ok { color:#166534; border-color:#86efac; background:#f0fdf4; }
.pill.off { color:#7f1d1d; border-color:#fca5a5; background:#fef2f2; }
.pill.warn { color:#9a3412; border-color:#fdba74; background:#fff7ed; }
.monitor-grid { display:grid; grid-template-columns:1fr; gap:8px; margin-bottom:10px; }
.monitor-box { border:1px solid #e2e8f0; border-radius:8px; padding:8px; background:#f8fafc; }
.monitor-box details { margin:0; }
.monitor-box summary { list-style:none; cursor:pointer; display:flex; align-items:center; justify-content:space-between; gap:8px; }
.monitor-box summary::-webkit-details-marker { display:none; }
.monitor-box summary::after { content:'+'; font-weight:700; color:#64748b; }
.monitor-box details[open] summary::after { content:'-'; }
.monitor-content { margin-top:8px; }
.badge-stack { display:flex; gap:6px; flex-wrap:wrap; }
.analyzer-list { display:grid; grid-template-columns:1fr; gap:8px; }
.analyzer-card { border:1px solid #dbe2ea; border-radius:8px; padding:8px; background:#fff; }
.analyzer-head { display:flex; justify-content:space-between; gap:8px; align-items:flex-start; margin-bottom:6px; }
.analyzer-meta { display:grid; grid-template-columns:1fr 1fr; gap:6px 10px; }
.analyzer-meta div { font-size:12px; color:#475569; }
.queue-list { display:grid; grid-template-columns:1fr; gap:6px; margin-top:6px; }
.queue-row { display:flex; justify-content:space-between; align-items:center; font-size:12px; gap:8px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace; }
.chart-wrap { margin-top:8px; border:1px solid #e2e8f0; border-radius:8px; padding:6px; background:#fff; }
.chart-legend { display:flex; gap:10px; align-items:center; font-size:11px; color:#64748b; margin:6px 0 2px; }
.dot { width:8px; height:8px; border-radius:50%; display:inline-block; }
.dot.new { background:#0284c7; }
.dot.disp { background:#16a34a; }
.dot.ratio { background:#9333ea; }
.requeue-panel { margin-bottom:10px; padding:10px; border:1px solid #e2e8f0; border-radius:10px; background:#f8fafc; display:none; }
.requeue-head { display:flex; gap:8px; align-items:center; justify-content:space-between; margin-bottom:6px; }
.requeue-flow { display:flex; align-items:center; gap:8px; margin-top:8px; flex-wrap:wrap; }
.flow-step { font-size:12px; padding:4px 8px; border-radius:999px; border:1px solid #cbd5e1; background:#fff; color:#475569; }
.flow-step.active { border-color:#38bdf8; color:#0369a1; background:#f0f9ff; }
.flow-step.done { border-color:#86efac; color:#166534; background:#f0fdf4; }
.flow-step.fail { border-color:#fca5a5; color:#7f1d1d; background:#fef2f2; }
.flow-link { width:18px; height:2px; background:#cbd5e1; border-radius:2px; }
.spinner { width:10px; height:10px; border-radius:50%; border:2px solid #bae6fd; border-top-color:#0284c7; animation:spin 800ms linear infinite; display:inline-block; margin-right:6px; vertical-align:middle; }
@keyframes spin { to { transform: rotate(360deg); } }
.table-wrap { max-height:70vh; overflow:auto; border:1px solid #e2e8f0; border-radius:8px; }
table { width:100%; border-collapse:collapse; font-size:12px; }
th, td { text-align:left; padding:8px; border-bottom:1px solid #e2e8f0; vertical-align:top; }
tr:hover { background:#f8fafc; }
tr.selected { background:#e2e8f0; }
button { border:1px solid #94a3b8; background:#fff; border-radius:6px; padding:5px 8px; cursor:pointer; font-size:12px; }
button.primary { background:#0f172a; color:#fff; border-color:#0f172a; }
.url { word-break:break-all; }
.modal-backdrop { position:fixed; inset:0; background:rgba(2,6,23,0.48); display:none; align-items:center; justify-content:center; z-index:40; padding:20px; }
.modal { width:min(980px, 96vw); max-height:90vh; overflow:auto; border:1px solid #22304a; border-radius:12px; background:#0b1220; color:#e5e7eb; }
.modal-head { display:flex; align-items:center; justify-content:space-between; gap:8px; padding:10px 12px; border-bottom:1px solid #22304a; position:sticky; top:0; background:#0b1220; }
.modal-title { font-size:13px; font-weight:600; color:#cbd5e1; }
.code { margin:0; padding:12px; font-size:12px; line-height:1.45; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", monospace; white-space:pre-wrap; word-break:break-word; }
.j-key { color:#7dd3fc; }
.j-str { color:#a7f3d0; }
.j-num { color:#fde68a; }
.j-bool { color:#fca5a5; }
.j-null { color:#c4b5fd; }
</style>
</head>
<body>
<header>
  <h1>RWCT Web Admin - SQLite Explorer</h1>
</header>
<main>
  <section class="card">
    <div class="row" style="justify-content:space-between; margin-bottom:10px;">
      <h2>Feed nel DB</h2>
      <button class="primary" onclick="reloadAll()">Aggiorna</button>
    </div>
    <div class="row" style="margin-bottom:10px;">
      <input id="newFeedUrl" class="url" placeholder="https://example.com/jobs.rss">
      <button onclick="addFeed()">Aggiungi feed</button>
    </div>
    <div class="monitor-box">
      <details>
        <summary><span class="small"><strong>Riepilogo pipeline</strong></span></summary>
        <div id="overview" class="monitor-content badge-stack small"></div>
      </details>
    </div>
    <div class="monitor-grid">
      <div class="monitor-box">
        <details>
          <summary><span class="small"><strong>Code SQLite (tempo reale)</strong></span></summary>
          <div class="monitor-content">
            <div id="mosqMeta" class="small"></div>
            <div id="queueList" class="queue-list"></div>
          </div>
        </details>
      </div>
      <div class="monitor-box">
        <details>
          <summary><span class="small"><strong>Analyzer attivi</strong></span></summary>
          <div id="analyzerList" class="monitor-content analyzer-list small"></div>
        </details>
      </div>
      <div class="monitor-box">
        <div class="small"><strong>Ratio NEW/DISPATCHED (ultime 3h, 1m)</strong></div>
        <div class="chart-legend">
          <span><span class="dot new"></span> NEW/min</span>
          <span><span class="dot disp"></span> DISPATCHED/min</span>
          <span><span class="dot ratio"></span> Ratio</span>
        </div>
        <div class="chart-wrap">
          <svg id="ratioChart" width="420" height="120" viewBox="0 0 420 120" preserveAspectRatio="none"></svg>
        </div>
      </div>
    </div>
    <div class="table-wrap">
      <table>
        <thead>
          <tr><th>Feed</th><th>Stato</th><th>Items</th><th>Azioni</th></tr>
        </thead>
        <tbody id="feedsBody"></tbody>
      </table>
    </div>
  </section>

  <section class="card">
    <h2>Items per feed</h2>
    <div id="selectedFeed" class="small" style="margin-bottom:10px;">Seleziona un feed per vedere gli items memorizzati.</div>
    <div id="requeuePanel" class="requeue-panel">
      <div class="requeue-head">
        <div>
          <strong>Requeue in corso</strong>
          <div id="requeueMeta" class="small"></div>
        </div>
        <div id="requeueStatusPill" class="pill warn">PENDING</div>
      </div>
      <div id="requeueStatusText" class="small"><span class="spinner"></span>Richiesta inviata, in attesa pipeline...</div>
      <div id="requeueError" class="small" style="color:#b91c1c; margin-top:4px;"></div>
      <div class="requeue-flow">
        <span id="flow-new" class="flow-step">NEW</span><span class="flow-link"></span>
        <span id="flow-analyzed" class="flow-step">ANALYZED</span><span class="flow-link"></span>
        <span id="flow-dispatched" class="flow-step">DISPATCHED</span>
      </div>
    </div>
    <div class="table-wrap">
      <table>
        <thead>
          <tr><th>Quando</th><th>Titolo</th><th>Status</th><th>Queue</th><th>URL</th><th>Hash</th><th>Azioni</th></tr>
        </thead>
        <tbody id="itemsBody"></tbody>
      </table>
    </div>
  </section>
</main>
<div id="jsonModalBackdrop" class="modal-backdrop" onclick="closeJSONModal(event)">
  <div class="modal" role="dialog" aria-modal="true" aria-labelledby="jsonModalTitle">
    <div class="modal-head">
      <div id="jsonModalTitle" class="modal-title">Processed version JSON</div>
      <button onclick="closeJSONModal()">Chiudi</button>
    </div>
    <pre id="jsonModalBody" class="code"></pre>
  </div>
</div>

<script>
let currentFeed = '';
let requeueInFlight = false;

async function api(path, opt={}) {
  const res = await fetch(path, Object.assign({headers:{'Content-Type':'application/json'}}, opt));
  const txt = await res.text();
  let data = {};
  try { data = txt ? JSON.parse(txt) : {}; } catch (_) { data = {error: txt}; }
  if (!res.ok) throw new Error(data.error || txt || ('HTTP ' + res.status));
  return data;
}

function fmtDate(v) {
  if (!v) return '-';
  return new Date(v).toLocaleString();
}

function fmtDateExact(v) {
  if (!v) return '-';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return '-';
  return d.toLocaleString('it-IT', { dateStyle: 'full', timeStyle: 'long' });
}

function fmtRelativeDate(v) {
  if (!v) return '-';
  const d = new Date(v);
  if (Number.isNaN(d.getTime())) return '-';
  const diffSec = Math.round((d.getTime() - Date.now()) / 1000);
  const absSec = Math.abs(diffSec);
  const rtf = new Intl.RelativeTimeFormat('it', { numeric: 'auto' });
  if (absSec < 60) return rtf.format(diffSec, 'second');
  const diffMin = Math.round(diffSec / 60);
  if (Math.abs(diffMin) < 60) return rtf.format(diffMin, 'minute');
  const diffHour = Math.round(diffMin / 60);
  if (Math.abs(diffHour) < 24) return rtf.format(diffHour, 'hour');
  const diffDay = Math.round(diffHour / 24);
  return rtf.format(diffDay, 'day');
}

function esc(v) {
  return String(v || '')
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');
}

function flowReset() {
  for (const id of ['flow-new', 'flow-analyzed', 'flow-dispatched']) {
    const el = document.getElementById(id);
    if (el) el.className = 'flow-step';
  }
}

function renderRequeueState(itemID, mode, status, err, updatedAt, active) {
  const panel = document.getElementById('requeuePanel');
  const meta = document.getElementById('requeueMeta');
  const pill = document.getElementById('requeueStatusPill');
  const text = document.getElementById('requeueStatusText');
  const errorEl = document.getElementById('requeueError');
  panel.style.display = 'block';
  meta.textContent = 'item: ' + itemID + ' · mode: ' + mode + ' · aggiornato: ' + fmtDate(updatedAt);
  errorEl.textContent = err || '';

  flowReset();
  const sNew = document.getElementById('flow-new');
  const sAnalyzed = document.getElementById('flow-analyzed');
  const sDispatched = document.getElementById('flow-dispatched');

  if (status === 'NEW') {
    sNew.className = 'flow-step active';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'Item in coda al reader/analyzer';
    pill.className = 'pill warn';
  } else if (status === 'ANALYZED') {
    sNew.className = 'flow-step done';
    sAnalyzed.className = 'flow-step active';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'Analisi completata, in attesa dispatch';
    pill.className = 'pill warn';
  } else if (status === 'DISPATCHED') {
    sNew.className = 'flow-step done';
    sAnalyzed.className = 'flow-step done';
    sDispatched.className = 'flow-step done';
    text.textContent = 'Pipeline completata: item inviato.';
    pill.className = 'pill ok';
  } else if (status === 'FAILED') {
    sNew.className = 'flow-step fail';
    sAnalyzed.className = 'flow-step fail';
    sDispatched.className = 'flow-step fail';
    text.textContent = 'Pipeline fallita.';
    pill.className = 'pill off';
  } else {
    sNew.className = 'flow-step active';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'Stato corrente: ' + (status || 'sconosciuto');
    pill.className = 'pill warn';
  }
  pill.textContent = status || 'PENDING';
}

function renderRatioChart(points) {
  const svg = document.getElementById('ratioChart');
  if (!svg) return;
  const w = 420;
  const h = 120;
  const pad = 8;
  if (!points || points.length === 0) {
    svg.innerHTML = '';
    return;
  }
  let maxCount = 1;
  let maxRatio = 1;
  for (const p of points) {
    if ((p.new_count || 0) > maxCount) maxCount = p.new_count || 0;
    if ((p.dispatched_count || 0) > maxCount) maxCount = p.dispatched_count || 0;
    if (typeof p.ratio === 'number' && p.ratio > maxRatio) maxRatio = p.ratio;
  }

  const stepX = (w - pad*2) / Math.max(1, points.length-1);
  let newPath = '';
  let dispPath = '';
  let ratioPath = '';
  points.forEach((p, i) => {
    const x = pad + i*stepX;
    const yNew = h - pad - ((p.new_count || 0) / maxCount) * (h - pad*2);
    const yDisp = h - pad - ((p.dispatched_count || 0) / maxCount) * (h - pad*2);
    const ratioVal = typeof p.ratio === 'number' ? p.ratio : 0;
    const yRatio = h - pad - (ratioVal / maxRatio) * (h - pad*2);
    newPath += (i === 0 ? 'M' : 'L') + x + ' ' + yNew + ' ';
    dispPath += (i === 0 ? 'M' : 'L') + x + ' ' + yDisp + ' ';
    ratioPath += (i === 0 ? 'M' : 'L') + x + ' ' + yRatio + ' ';
  });

  svg.innerHTML =
    '<line x1="' + pad + '" y1="' + (h-pad) + '" x2="' + (w-pad) + '" y2="' + (h-pad) + '" stroke="#e2e8f0" stroke-width="1" />' +
    '<path d="' + newPath + '" fill="none" stroke="#0284c7" stroke-width="1.8" />' +
    '<path d="' + dispPath + '" fill="none" stroke="#16a34a" stroke-width="1.8" />' +
    '<path d="' + ratioPath + '" fill="none" stroke="#9333ea" stroke-width="1.3" stroke-dasharray="3 2" />';
}

async function loadQueueMonitor() {
  const data = await api('/api/monitor/queues');
  const mosqMeta = document.getElementById('mosqMeta');
  const list = document.getElementById('queueList');
  const p = data.pipeline || {};
  const rawBlocked = p.consumer_blocked_raw ? ' <span class="pill off">RAW blocked?</span>' : '';
  const analyzedBlocked = p.consumer_blocked_analyzed ? ' <span class="pill off">ANALYZED blocked?</span>' : '';
  mosqMeta.innerHTML =
    'Dispatched total: <span class="mono">' + (p.dispatched_total || 0) + '</span>' +
    ' · new(5m): <span class="mono">' + (p.new_last_5m || 0) + '</span>' +
    ' · dispatched(5m): <span class="mono">' + (p.dispatched_last_5m || 0) + '</span>' +
    '<br>Stuck>10m NEW: <span class="mono">' + (p.stuck_new_10m || 0) + '</span>' +
    ' · Stuck>10m ANALYZED: <span class="mono">' + (p.stuck_analyzed_10m || 0) + '</span>' +
    rawBlocked + analyzedBlocked;

  const queues = data.queues || [];
  list.innerHTML = queues.map(q =>
    '<div class="queue-row"><span><strong>' + q.name + '</strong> <span class="small mono">' + q.topic + '</span></span>' +
    '<span class="pill">' + q.queued + '</span></div>'
  ).join('');

  renderRatioChart((data.ratio_window && data.ratio_window.points) || []);
}

async function loadOverview() {
  const o = await api('/api/db/overview');
  const el = document.getElementById('overview');
  el.innerHTML =
    '<span class="pill ok">Feeds ON: ' + o.feeds_enabled + '</span>' +
    '<span class="pill off">Feeds OFF: ' + o.feeds_disabled + '</span>' +
    '<span class="pill">NEW: ' + o.pending_items + '</span>' +
    '<span class="pill">DISPATCHED: ' + o.processed_items + '</span>' +
    '<span class="pill">ANALYZED: ' + o.analyzed_items + '</span>';
}

async function loadAnalyzerRuntime() {
  const data = await api('/api/runtime/analyzers');
  const list = document.getElementById('analyzerList');
  const analyzers = data.analyzers || [];
  list.innerHTML = analyzers.map(a =>
    '<div class="analyzer-card">' +
      '<div class="analyzer-head">' +
        '<div><strong>' + esc(a.name || 'job-analyzer') + '</strong><div class="small mono">' + esc(a.model || '-') + '</div></div>' +
        '<span class="pill ' + (a.thinking_enabled ? 'ok' : 'warn') + '">thinking: ' + (a.thinking_enabled ? 'on' : 'off') + '</span>' +
      '</div>' +
      '<div class="badge-stack" style="margin-bottom:8px;">' +
        '<span class="pill">max_tokens: ' + (a.max_tokens || 0) + '</span>' +
        '<span class="pill">timeout: ' + esc(a.timeout || '-') + '</span>' +
        '<span class="pill">max_jobs/min: ' + (a.max_jobs_per_min || 0) + '</span>' +
        '<span class="pill">workers: ' + (a.max_concurrency || 0) + '</span>' +
      '</div>' +
      '<div class="analyzer-meta">' +
        '<div><strong>Endpoint</strong><br><span class="mono">' + esc(a.endpoint || '-') + '</span></div>' +
        '<div><strong>Lease</strong><br><span class="mono">' + esc(a.lease_duration || '-') + '</span></div>' +
        '<div><strong>Poll interval</strong><br><span class="mono">' + esc(a.queue_poll_interval || '-') + '</span></div>' +
        '<div><strong>Profile</strong><br><span class="mono">env-configured</span></div>' +
      '</div>' +
    '</div>'
  ).join('');
}

async function loadFeeds() {
  const feeds = await api('/api/db/feeds');
  const body = document.getElementById('feedsBody');
  body.innerHTML = '';

  for (const f of feeds) {
    const tr = document.createElement('tr');
    if (f.url === currentFeed) tr.classList.add('selected');
    const enabledPill = f.enabled
      ? '<span class="pill ok">enabled</span>'
      : '<span class="pill off">disabled</span>';
    const health = f.health_status || (f.enabled ? 'ok' : 'disabled');
    const healthPillClass = health === 'ok' ? 'ok' : (health === 'broken' ? 'off' : 'warn');
    const healthPillLabel = health === 'ok' ? 'health: ok' : (health === 'broken' ? 'health: rotto' : 'health: n/a');
    const refresh = f.last_refresh_at
      ? '<div class="small" title="' + esc(fmtDateExact(f.last_refresh_at)) + '">ultimo refresh ' + fmtRelativeDate(f.last_refresh_at) + '</div>'
      : '<div class="small">ultimo refresh: mai</div>';
    const down = f.down_until ? ('<div class="small">down until ' + fmtDate(f.down_until) + '</div>') : '';
    const reason = f.health_reason ? ('<div class="small">' + esc(f.health_reason) + '</div>') : '';
    const toggleLabel = f.enabled ? 'Disabilita' : 'Abilita';
    tr.innerHTML =
      '<td class="url"><div>' + f.url + '</div><div class="small">label: ' + f.source_label + '</div></td>' +
      '<td>' + enabledPill + ' <span class="pill ' + healthPillClass + '">' + healthPillLabel + '</span>' + refresh + down + reason + '</td>' +
      '<td>' + f.items_count + '</td>' +
      '<td class="row"><button onclick="toggleFeed(\'' + encodeURIComponent(f.url) + '\',' + (!f.enabled) + '); event.stopPropagation();">' + toggleLabel + '</button><button onclick="forcePollFeed(\'' + encodeURIComponent(f.url) + '\'); event.stopPropagation();">Forza poll</button><button onclick="removeFeed(\'' + encodeURIComponent(f.url) + '\'); event.stopPropagation();">Rimuovi</button></td>';
    tr.onclick = () => selectFeed(f.url, f.source_label, f.items_count);
    body.appendChild(tr);
  }
}

async function addFeed() {
  const input = document.getElementById('newFeedUrl');
  const url = (input.value || '').trim();
  if (!url) return;
  await api('/api/db/feeds', {
    method: 'POST',
    body: JSON.stringify({url})
  });
  input.value = '';
  await reloadAll();
}

async function toggleFeed(encodedUrl, enabled) {
  const url = decodeURIComponent(encodedUrl);
  await api('/api/db/feeds/toggle', {
    method: 'POST',
    body: JSON.stringify({url, enabled})
  });
  await loadFeeds();
  await loadOverview();
}

async function forcePollFeed(encodedUrl) {
  const url = decodeURIComponent(encodedUrl);
  await api('/api/db/feeds/poll', {
    method: 'POST',
    body: JSON.stringify({url})
  });
  await reloadAll();
}

async function removeFeed(encodedUrl) {
  const url = decodeURIComponent(encodedUrl);
  if (!confirm('Rimuovere definitivamente il feed dal DB?\\n' + url)) return;
  await api('/api/db/feeds?url=' + encodeURIComponent(url), { method: 'DELETE' });
  if (currentFeed === url) {
    currentFeed = '';
    document.getElementById('selectedFeed').innerHTML = 'Seleziona un feed per vedere gli items memorizzati.';
    document.getElementById('itemsBody').innerHTML = '';
  }
  await reloadAll();
}

async function selectFeed(url, label, count) {
  currentFeed = url;
  await loadFeeds();

  const data = await api('/api/db/feed-items?feed_url=' + encodeURIComponent(url) + '&limit=200');
  const effectiveLabel = data.source_label || label || '-';
  const effectiveCount = (typeof count === 'number' && count > 0) ? count : ((data.items || []).length || 0);
  document.getElementById('selectedFeed').innerHTML =
    '<div><strong>' + url + '</strong></div><div class="small">source_label: ' + effectiveLabel + ' · db items: ' + effectiveCount + '</div>';
  const body = document.getElementById('itemsBody');
  body.innerHTML = '';
  for (const it of (data.items || [])) {
    const tr = document.createElement('tr');
    const title = it.title || '(senza titolo)';
    tr.innerHTML =
      '<td>' + fmtDate(it.created_at) + '</td>' +
      '<td>' + title + '</td>' +
      '<td>' + (it.status || '-') + '</td>' +
      '<td><span class="pill">' + (it.queue_state || '-') + '</span></td>' +
      '<td class="url"><a href="' + (it.url || '#') + '" target="_blank" rel="noreferrer">' + (it.url || '-') + '</a></td>' +
      '<td class="url">' + (it.item_hash || '-') + '</td>' +
      '<td class="row">' +
        '<button onclick="requeueItem(\'' + encodeURIComponent(it.id) + '\')"' + (it.has_analyzed_version ? '' : ' disabled title="Serve una processed version"') + '>Requeue</button>' +
        '<button onclick="showAnalyzedVersion(\'' + encodeURIComponent(it.id) + '\')"' + (it.has_analyzed_version ? '' : ' disabled') + '>Processed version</button>' +
        '<button onclick="deleteItem(\'' + encodeURIComponent(it.id) + '\')">Elimina</button>' +
      '</td>';
    body.appendChild(tr);
  }
}

async function requeueItem(encodedId) {
  if (requeueInFlight) return;
  requeueInFlight = true;
  const id = decodeURIComponent(encodedId);
  renderRequeueState(id, 'pending', 'NEW', '', new Date().toISOString(), true);
  try {
    const res = await api('/api/db/items/requeue', { method: 'POST', body: JSON.stringify({id}) });
    const mode = res.mode || 'raw';
    const started = Date.now();
    while (Date.now()-started < 90000) {
      const st = await api('/api/db/items/status?id=' + encodeURIComponent(id));
      const active = st.status !== 'DISPATCHED' && st.status !== 'FAILED';
      renderRequeueState(id, mode, st.status, st.last_error || '', st.updated_at, active);
      if (!active) break;
      await new Promise(r => setTimeout(r, 1200));
    }
  } catch (err) {
    renderRequeueState(id, 'error', 'FAILED', err.message || String(err), new Date().toISOString(), false);
    throw err;
  } finally {
    requeueInFlight = false;
    await reloadAll();
  }
}

async function deleteItem(encodedId) {
  const id = decodeURIComponent(encodedId);
  if (!confirm('Eliminare item dal DB?\\n' + id)) return;
  await api('/api/db/items?id=' + encodeURIComponent(id), { method: 'DELETE' });
  await reloadAll();
}

async function showAnalyzedVersion(encodedId) {
  const id = decodeURIComponent(encodedId);
  const res = await api('/api/db/items/analyzed?id=' + encodeURIComponent(id));
  if (!res.analyzed_payload_json) {
    alert('Nessuna processed version disponibile per questo item.');
    return;
  }
  showJSONModal(id, res.analyzed_payload_json);
}

function syntaxHighlightJSON(v) {
  const escText = esc(v);
  return escText
    .replace(/\"([^\"\\]|\\.)*\"(?=\\s*:)/g, '<span class="j-key">$&</span>')
    .replace(/(:\\s*)\"([^\"\\]|\\.)*\"/g, '$1<span class="j-str">"$2"</span>')
    .replace(/\\b(true|false)\\b/g, '<span class="j-bool">$1</span>')
    .replace(/\\bnull\\b/g, '<span class="j-null">null</span>')
    .replace(/-?\\b\\d+(?:\\.\\d+)?(?:[eE][+\\-]?\\d+)?\\b/g, '<span class="j-num">$&</span>');
}

function showJSONModal(id, rawPayload) {
  let formatted = rawPayload;
  try {
    formatted = JSON.stringify(JSON.parse(rawPayload), null, 2);
  } catch (_) {}
  document.getElementById('jsonModalTitle').textContent = 'Processed version JSON · ' + id;
  document.getElementById('jsonModalBody').innerHTML = syntaxHighlightJSON(formatted);
  document.getElementById('jsonModalBackdrop').style.display = 'flex';
}

function closeJSONModal(ev) {
  if (ev && ev.target && ev.target.id !== 'jsonModalBackdrop') return;
  document.getElementById('jsonModalBackdrop').style.display = 'none';
}

async function reloadAll() {
  await loadOverview();
  await loadAnalyzerRuntime();
  await loadQueueMonitor();
  await loadFeeds();
  if (currentFeed) {
    const selected = currentFeed;
    currentFeed = '';
    await selectFeed(selected, '', 0);
  }
}

reloadAll().catch(err => {
  console.error(err);
  alert(err.message || String(err));
});

setInterval(() => {
  loadQueueMonitor().catch(err => console.error(err));
}, 5000);

setInterval(() => {
  loadAnalyzerRuntime().catch(err => console.error(err));
}, 30000);

setInterval(() => {
  loadFeeds().catch(err => console.error(err));
}, 30000);
</script>
</body>
</html>`

func MustEnvDuration(key, def string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		v = def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		panic(fmt.Sprintf("invalid duration for %s: %v", key, err))
	}
	return d
}
