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

	metricsReg          otelmetric.Registration
	queueItemsGauge     otelmetric.Int64ObservableGauge
	itemStatusGauge     otelmetric.Int64ObservableGauge
	itemQueueStateGauge otelmetric.Int64ObservableGauge
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

type boardLaneRow struct {
	Name  string         `json:"name"`
	Title string         `json:"title"`
	Count int64          `json:"count"`
	Items []boardItemRow `json:"items"`
}

type boardItemRow struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	URL           string    `json:"url"`
	Status        string    `json:"status"`
	Lane          string    `json:"lane"`
	QueueState    string    `json:"queue_state"`
	SourceFeedURL string    `json:"source_feed_url"`
	SourceLabel   string    `json:"source_label"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastError     string    `json:"last_error,omitempty"`
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
	mux.HandleFunc("/api/monitor/board", s.handleBoardMonitor)
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
	var sourceFeedURL, guid, rawURL, title, description, linksJSON, imagesJSON string
	var publishedRaw, fetchedRaw sql.NullString
	err := s.db.QueryRow(
		`SELECT
			COALESCE(analyzed_payload_json,''),
			COALESCE(source_feed_url,''),
			COALESCE(guid,''),
			COALESCE(url,''),
			COALESCE(title,''),
			COALESCE(description,''),
			COALESCE(links_json,'[]'),
			COALESCE(image_urls_json,'[]'),
			CAST(published_at AS TEXT),
			CAST(fetched_at AS TEXT)
FROM rss_items WHERE id = ?`,
		itemID,
	).Scan(
		&analyzedJSON,
		&sourceFeedURL,
		&guid,
		&rawURL,
		&title,
		&description,
		&linksJSON,
		&imagesJSON,
		&publishedRaw,
		&fetchedRaw,
	)
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
		var links, images []string
		_ = json.Unmarshal([]byte(linksJSON), &links)
		_ = json.Unmarshal([]byte(imagesJSON), &images)

		publishedAt := now
		if publishedRaw.Valid {
			if t, ok := parseDBTime(publishedRaw.String); ok {
				publishedAt = t.Time.UTC()
			}
		}
		fetchedAt := now
		if fetchedRaw.Valid {
			if t, ok := parseDBTime(fetchedRaw.String); ok {
				fetchedAt = t.Time.UTC()
			}
		}

		rawEvent := events.RawJobItem{
			SchemaVersion: events.SchemaVersion,
			ID:            itemID,
			FeedURL:       strings.TrimSpace(sourceFeedURL),
			SourceLabel:   sourceLabelFromFeedURL(sourceFeedURL),
			GUID:          strings.TrimSpace(guid),
			URL:           strings.TrimSpace(rawURL),
			Title:         strings.TrimSpace(title),
			Description:   strings.TrimSpace(description),
			Links:         links,
			ImageURLs:     images,
			PublishedAt:   publishedAt,
			FetchedAt:     fetchedAt,
		}
		payload, err := json.Marshal(rawEvent)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, err = s.db.Exec(
			`INSERT INTO analyzer_queue(item_id, payload_json, state, enqueued_at, updated_at)
VALUES(?, ?, 'QUEUED', ?, ?)
ON CONFLICT(item_id) DO UPDATE SET
 payload_json=excluded.payload_json,
 state='QUEUED',
 lease_owner=NULL,
 lease_until=NULL,
 last_error='',
 updated_at=excluded.updated_at`,
			itemID, string(payload), now, now,
		)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		_, _ = s.db.Exec(
			`UPDATE rss_items
			 SET status=?,
			     raw_enqueued_at=COALESCE(?, raw_enqueued_at),
			     raw_enqueue_count=COALESCE(raw_enqueue_count,0)+1,
			     last_error='',
			     dispatched_at=NULL,
			     updated_at=?
			 WHERE id=?`,
			events.ItemStatusNew, now, now, itemID,
		)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": "raw"})
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

func (s *Service) handleBoardMonitor(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	limitPerLane := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit_per_lane")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limitPerLane = n
		}
	}

	rows, err := s.db.Query(`
SELECT
 i.id,
 i.title,
 i.url,
 i.status,
 CASE
   WHEN aq.state = 'LEASED' THEN 'raw_inflight'
   WHEN aq.state = 'QUEUED' THEN 'raw_backlog'
   WHEN dq.state = 'LEASED' THEN 'analyzed_inflight'
   WHEN dq.state = 'QUEUED' THEN 'analyzed_backlog'
   WHEN i.status = 'FAILED' THEN 'failed'
   WHEN i.status = 'DISPATCHED' THEN ''
   WHEN i.status = 'ANALYZED' AND dq.state = 'DONE' THEN 'anomalies'
   WHEN i.status = 'NEW' AND aq.state = 'DONE' THEN 'anomalies'
   WHEN i.status = 'ANALYZED' AND dq.item_id IS NULL THEN 'anomalies'
   WHEN i.status = 'NEW' AND aq.item_id IS NULL THEN 'anomalies'
   ELSE ''
 END AS lane,
 CASE
   WHEN aq.state = 'LEASED' THEN 'inflight_raw'
   WHEN aq.state = 'QUEUED' THEN 'queued_raw'
   WHEN aq.state = 'DONE' THEN 'processed_raw'
   WHEN dq.state = 'LEASED' THEN 'inflight_analyzed'
   WHEN dq.state = 'QUEUED' THEN 'queued_analyzed'
   WHEN dq.state = 'DONE' THEN 'processed_analyzed'
   WHEN i.status = 'NEW' AND aq.item_id IS NULL THEN 'not_enqueued_raw'
   WHEN i.status = 'ANALYZED' AND dq.item_id IS NULL THEN 'not_enqueued_analyzed'
   WHEN i.status = 'DISPATCHED' THEN 'dispatched'
   WHEN i.status = 'FAILED' THEN 'failed'
   ELSE 'unknown'
 END AS queue_state,
 i.source_feed_url,
 i.created_at,
 i.updated_at,
 COALESCE(i.last_error,'') AS last_error,
 CASE
   WHEN i.status = 'NEW' THEN COALESCE(aq.enqueued_at, i.created_at, i.updated_at)
   WHEN i.status = 'ANALYZED' THEN COALESCE(dq.enqueued_at, i.analyzed_enqueued_at, i.updated_at, i.created_at)
   ELSE COALESCE(i.updated_at, i.created_at)
 END AS fifo_ts
FROM rss_items i
LEFT JOIN analyzer_queue aq ON aq.item_id = i.id
LEFT JOIN dispatch_queue dq ON dq.item_id = i.id
ORDER BY
 CASE
   WHEN aq.state = 'QUEUED' THEN 1
   WHEN aq.state = 'LEASED' THEN 2
   WHEN dq.state = 'QUEUED' THEN 3
   WHEN dq.state = 'LEASED' THEN 4
   WHEN i.status = 'FAILED' THEN 5
   ELSE 6
 END,
 fifo_ts ASC,
 i.created_at ASC
`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer rows.Close()

	laneOrder := []string{"raw_backlog", "raw_inflight", "analyzed_backlog", "analyzed_inflight", "failed", "anomalies"}
	laneTitles := map[string]string{
		"raw_backlog":       "Raw Backlog",
		"raw_inflight":      "Raw Inflight",
		"analyzed_backlog":  "Analyzed Backlog",
		"analyzed_inflight": "Analyzed Inflight",
		"failed":            "Failed",
		"anomalies":         "Anomalies",
	}
	laneCounts := map[string]int64{}
	laneItems := map[string][]boardItemRow{}
	for _, ln := range laneOrder {
		laneCounts[ln] = 0
		laneItems[ln] = make([]boardItemRow, 0)
	}

	for rows.Next() {
		var it boardItemRow
		var lane string
		var sourceFeedURL sql.NullString
		if err := rows.Scan(
			&it.ID,
			&it.Title,
			&it.URL,
			&it.Status,
			&lane,
			&it.QueueState,
			&sourceFeedURL,
			&it.CreatedAt,
			&it.UpdatedAt,
			&it.LastError,
			new(sql.NullString), // fifo_ts ignored in payload, used only for sorting in SQL
		); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if lane == "" {
			continue
		}
		it.Lane = lane
		if sourceFeedURL.Valid {
			it.SourceFeedURL = sourceFeedURL.String
		}
		it.SourceLabel = sourceLabelFromFeedURL(it.SourceFeedURL)

		laneCounts[lane]++
		if len(laneItems[lane]) < limitPerLane {
			laneItems[lane] = append(laneItems[lane], it)
		}
	}
	if err := rows.Err(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	out := make([]boardLaneRow, 0, len(laneOrder))
	for _, ln := range laneOrder {
		out = append(out, boardLaneRow{
			Name:  ln,
			Title: laneTitles[ln],
			Count: laneCounts[ln],
			Items: laneItems[ln],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"limit_per_lane": limitPerLane,
		"lanes":          out,
	})
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
<title>RWCT Web Admin</title>
<style>
:root {
  color-scheme: light;
  --bg: #f3f6fb;
  --card: #ffffff;
  --border: #d6deeb;
  --text: #1d2939;
  --muted: #667085;
  --accent: #0f172a;
}
* { box-sizing: border-box; }
html, body { margin: 0; height: 100%; font-family: "Segoe UI", Tahoma, sans-serif; color: var(--text); background: var(--bg); }
.app { display: grid; grid-template-columns: 260px 1fr; min-height: 100vh; transition: grid-template-columns .2s ease; }
.app.sidebar-collapsed { grid-template-columns: 64px 1fr; }
.sidebar { background: #0b1220; color: #e2e8f0; border-right: 1px solid #1f2937; padding: 10px; display: flex; flex-direction: column; gap: 10px; }
.brand { display: flex; align-items: center; justify-content: space-between; gap: 6px; }
.brand-title { font-size: 14px; font-weight: 700; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.collapse-btn { border: 1px solid #334155; color: #e2e8f0; background: #0f172a; border-radius: 8px; padding: 6px 8px; cursor: pointer; }
.view-nav { display: grid; gap: 6px; }
.view-btn { text-align: left; border: 1px solid #1f2937; background: transparent; color: #e2e8f0; border-radius: 10px; padding: 10px 10px; cursor: pointer; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
.view-btn.active { background: #1e293b; border-color: #334155; }
.sidebar-collapsed .brand-title, .sidebar-collapsed .view-label, .sidebar-collapsed .sidebar-footer { display: none; }
.content { padding: 14px; display: grid; gap: 12px; }
.topbar { display: flex; align-items: center; justify-content: space-between; gap: 10px; }
.top-title { font-size: 18px; font-weight: 700; margin: 0; }
.card { background: var(--card); border: 1px solid var(--border); border-radius: 12px; padding: 10px; }
.small { font-size: 12px; color: var(--muted); }
.row { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.pill { font-size: 11px; padding: 2px 8px; border-radius: 999px; border: 1px solid #cbd5e1; background: #f8fafc; }
.pill.ok { color: #166534; border-color: #86efac; background: #f0fdf4; }
.pill.warn { color: #9a3412; border-color: #fdba74; background: #fff7ed; }
.pill.off { color: #7f1d1d; border-color: #fca5a5; background: #fef2f2; }
button { border: 1px solid #94a3b8; border-radius: 8px; background: #fff; padding: 6px 8px; cursor: pointer; }
button.primary { background: var(--accent); color: #fff; border-color: var(--accent); }
button.tiny { padding: 4px 6px; font-size: 11px; }
input { border: 1px solid #cbd5e1; border-radius: 8px; padding: 6px 8px; }
.mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; }
.views { display: grid; gap: 10px; }
.view { display: none; }
.view.active { display: block; }
.feed-layout { display: grid; grid-template-columns: 420px 1fr; gap: 10px; }
.table-wrap { max-height: 70vh; overflow: auto; border: 1px solid var(--border); border-radius: 10px; }
table { width: 100%; border-collapse: collapse; font-size: 12px; }
th, td { border-bottom: 1px solid #e2e8f0; text-align: left; padding: 8px; vertical-align: top; }
tr:hover { background: #f8fafc; }
tr.selected { background: #e2e8f0; }
.url { word-break: break-all; }
.kanban-shell { display: grid; gap: 10px; }
.lanes { display: grid; grid-auto-flow: column; grid-auto-columns: minmax(280px, 1fr); gap: 10px; overflow-x: auto; padding-bottom: 4px; }
.lane { border: 1px solid var(--border); border-radius: 12px; background: #fff; min-height: 60vh; display: grid; grid-template-rows: auto 1fr; }
.lane-head { padding: 10px; border-bottom: 1px solid var(--border); background: #f8fafc; display: flex; justify-content: space-between; align-items: center; gap: 8px; position: sticky; top: 0; z-index: 1; }
.lane-title { font-size: 13px; font-weight: 700; }
.lane-items { padding: 8px; display: grid; gap: 8px; align-content: start; }
.item-card { border: 1px solid #dbe2ea; border-radius: 10px; padding: 8px; background: #fff; display: grid; gap: 6px; }
.item-title { font-size: 13px; font-weight: 600; line-height: 1.3; }
.item-meta { font-size: 11px; color: var(--muted); display: grid; gap: 2px; }
.item-actions { display: flex; gap: 6px; flex-wrap: wrap; }
.muted { color: var(--muted); }
.requeue-panel { margin-bottom: 10px; padding: 10px; border: 1px solid var(--border); border-radius: 10px; background: #f8fafc; display: none; }
.requeue-flow { display: flex; align-items: center; gap: 8px; margin-top: 8px; flex-wrap: wrap; }
.flow-step { font-size: 12px; padding: 4px 8px; border-radius: 999px; border: 1px solid #cbd5e1; background: #fff; color: #475569; }
.flow-step.active { border-color: #38bdf8; color: #0369a1; background: #f0f9ff; }
.flow-step.done { border-color: #86efac; color: #166534; background: #f0fdf4; }
.flow-step.fail { border-color: #fca5a5; color: #7f1d1d; background: #fef2f2; }
.flow-link { width: 18px; height: 2px; background: #cbd5e1; border-radius: 2px; }
.spinner { width: 10px; height: 10px; border-radius: 50%; border: 2px solid #bae6fd; border-top-color: #0284c7; animation: spin 800ms linear infinite; display: inline-block; margin-right: 6px; vertical-align: middle; }
@keyframes spin { to { transform: rotate(360deg); } }
.modal-backdrop { position: fixed; inset: 0; background: rgba(2,6,23,.5); display: none; align-items: center; justify-content: center; z-index: 40; padding: 20px; }
.modal { width: min(980px, 96vw); max-height: 90vh; overflow: auto; border: 1px solid #22304a; border-radius: 12px; background: #0b1220; color: #e5e7eb; }
.modal-head { display: flex; align-items: center; justify-content: space-between; padding: 10px 12px; border-bottom: 1px solid #22304a; position: sticky; top: 0; background: #0b1220; }
.code { margin: 0; padding: 12px; font-size: 12px; line-height: 1.45; font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; white-space: pre-wrap; word-break: break-word; }
.j-key { color: #7dd3fc; }
.j-num { color: #fde68a; }
.j-bool { color: #fca5a5; }
.j-null { color: #c4b5fd; }
@media (max-width: 1200px) { .feed-layout { grid-template-columns: 1fr; } }
</style>
</head>
<body>
<div id="app" class="app">
  <aside class="sidebar">
    <div class="brand">
      <div class="brand-title">RWCT Web Admin</div>
      <button class="collapse-btn" onclick="toggleSidebar()">☰</button>
    </div>
    <nav id="viewNav" class="view-nav"></nav>
    <div class="sidebar-footer small">Nuove viste: aggiungi un elemento nel registry JS.</div>
  </aside>

  <main class="content">
    <div class="topbar card">
      <h1 id="currentViewTitle" class="top-title">Feeds</h1>
      <div class="row">
        <button class="primary" onclick="reloadCurrentView()">Aggiorna vista</button>
        <span id="lastRefresh" class="small"></span>
      </div>
    </div>

    <div id="globalSummary" class="card row small"></div>

    <div class="views">
      <section id="view-feeds" class="view active">
        <div class="feed-layout">
          <div class="card">
            <div class="row" style="justify-content:space-between;margin-bottom:8px;">
              <strong>RSS Feeds</strong>
              <span class="small">Seleziona feed per vedere gli item e agire</span>
            </div>
            <div class="row" style="margin-bottom:8px;">
              <input id="newFeedUrl" class="url" placeholder="https://example.com/jobs.rss" style="flex:1;min-width:220px;">
              <button onclick="addFeed()">Aggiungi</button>
            </div>
            <div class="table-wrap">
              <table id="feedTable">
                <thead><tr><th>Feed</th><th>Stato</th><th>Items</th><th>Azioni</th></tr></thead>
                <tbody></tbody>
              </table>
            </div>
          </div>

          <div class="card">
            <div id="selectedFeed" class="small" style="margin-bottom:8px;">Seleziona un feed per vedere gli items memorizzati.</div>
            <div id="requeuePanel" class="requeue-panel">
              <div class="row" style="justify-content:space-between;">
                <div class="small"><strong>Requeue monitor</strong> <span id="requeueItemLabel" class="mono"></span></div>
                <div id="requeueStatusPill" class="pill warn">PENDING</div>
              </div>
              <div id="requeueStatusText" class="small"><span class="spinner"></span>Richiesta inviata, in attesa pipeline...</div>
              <div class="requeue-flow">
                <span id="stepQueued" class="flow-step">NEW</span><span class="flow-link"></span>
                <span id="stepAnalyzed" class="flow-step">ANALYZED</span><span class="flow-link"></span>
                <span id="stepDispatched" class="flow-step">DISPATCHED</span>
              </div>
            </div>
            <div class="table-wrap">
              <table id="itemsTable">
                <thead><tr><th>Quando</th><th>Titolo</th><th>Status</th><th>Queue</th><th>URL</th><th>Azioni</th></tr></thead>
                <tbody></tbody>
              </table>
            </div>
          </div>
        </div>
      </section>

      <section id="view-board" class="view">
        <div class="card kanban-shell">
          <div class="row" style="justify-content:space-between;">
            <div>
              <strong>Pipeline Board</strong>
              <span class="small">FIFO: in alto gli item che verranno prelevati prima.</span>
            </div>
            <div class="row">
              <label class="small">Limit/lane</label>
              <input id="boardLimit" value="120" style="width:80px;">
              <button id="boardAutoBtn" onclick="toggleBoardAutoRefresh()">Auto-refresh: ON</button>
              <button onclick="loadBoard()">Refresh now</button>
            </div>
          </div>
          <div id="boardMeta" class="small"></div>
          <div id="boardLanes" class="lanes"></div>
        </div>
      </section>
    </div>
  </main>
</div>

<div id="jsonModalBackdrop" class="modal-backdrop" onclick="closeJSONModal(event)">
  <div class="modal">
    <div class="modal-head">
      <div>Versione analizzata JSON - <span id="jsonModalID" class="mono"></span></div>
      <button onclick="closeJSONModal()">Chiudi</button>
    </div>
    <pre id="jsonModalBody" class="code"></pre>
  </div>
</div>

<script>
var state = {
  currentView: 'feeds',
  currentFeed: '',
  sidebarCollapsed: false,
  boardAutoRefresh: true,
  boardTimer: null,
  requeuePollTimer: null
};

var VIEW_REGISTRY = [
  { id: 'feeds', label: 'Feeds & Items', title: 'Feeds' },
  { id: 'board', label: 'Pipeline Board', title: 'Pipeline Board' }
];

async function api(path, opt) {
  var res = await fetch(path, opt || {});
  if (!res.ok) {
    var txt = await res.text();
    var data = {};
    try { data = JSON.parse(txt); } catch (_) {}
    throw new Error(data.error || txt || ('HTTP ' + res.status));
  }
  return res.json();
}

function esc(v) {
  return String(v == null ? '' : v).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('>','&gt;').replaceAll('"','&quot;').replaceAll("'","&#39;");
}

function fmtDate(v) {
  if (!v) return '-';
  try { return new Date(v).toLocaleString(); } catch (_) { return String(v); }
}

function fmtRelative(v) {
  if (!v) return '-';
  var t = new Date(v).getTime();
  if (!Number.isFinite(t)) return fmtDate(v);
  var d = Date.now() - t;
  var a = Math.abs(d);
  var s = Math.floor(a / 1000);
  if (s < 60) return (d >= 0 ? s + 's fa' : 'tra ' + s + 's');
  var m = Math.floor(s / 60);
  if (m < 60) return (d >= 0 ? m + 'm fa' : 'tra ' + m + 'm');
  var h = Math.floor(m / 60);
  if (h < 24) return (d >= 0 ? h + 'h fa' : 'tra ' + h + 'h');
  var g = Math.floor(h / 24);
  return (d >= 0 ? g + 'g fa' : 'tra ' + g + 'g');
}

function setLastRefresh() {
  document.getElementById('lastRefresh').textContent = 'Ultimo refresh: ' + new Date().toLocaleTimeString();
}

function renderViewMenu() {
  var nav = document.getElementById('viewNav');
  nav.innerHTML = '';
  VIEW_REGISTRY.forEach(function(v){
    var b = document.createElement('button');
    b.className = 'view-btn' + (state.currentView === v.id ? ' active' : '');
    b.innerHTML = '<span class="view-label">' + esc(v.label) + '</span>';
    b.onclick = function(){ switchView(v.id); };
    nav.appendChild(b);
  });
}

function toggleSidebar() {
  state.sidebarCollapsed = !state.sidebarCollapsed;
  document.getElementById('app').classList.toggle('sidebar-collapsed', state.sidebarCollapsed);
}

async function switchView(id) {
  state.currentView = id;
  VIEW_REGISTRY.forEach(function(v){
    var el = document.getElementById('view-' + v.id);
    if (el) el.classList.toggle('active', v.id === id);
  });
  renderViewMenu();
  var selected = VIEW_REGISTRY.find(function(v){ return v.id === id; });
  document.getElementById('currentViewTitle').textContent = selected ? selected.title : id;
  if (id === 'board') {
    await loadBoard();
    startBoardAutoRefresh();
  } else {
    stopBoardAutoRefresh();
    await loadFeedsView();
  }
  setLastRefresh();
}

async function loadGlobalSummary() {
  var overview = await api('/api/db/overview');
  var queues = await api('/api/monitor/queues');
  var q = queues.queues || [];
  var qMap = {};
  q.forEach(function(x){ qMap[x.name] = Number(x.queued || 0); });
  var p = queues.pipeline || {};
  document.getElementById('globalSummary').innerHTML = ''
    + '<span class="pill">feeds on: ' + Number(overview.feeds_enabled || 0) + '</span>'
    + '<span class="pill">feeds off: ' + Number(overview.feeds_disabled || 0) + '</span>'
    + '<span class="pill">raw_backlog: ' + (qMap.raw_backlog || 0) + '</span>'
    + '<span class="pill">raw_inflight: ' + (qMap.raw_inflight || 0) + '</span>'
    + '<span class="pill">analyzed_backlog: ' + (qMap.analyzed_backlog || 0) + '</span>'
    + '<span class="pill">analyzed_inflight: ' + (qMap.analyzed_inflight || 0) + '</span>'
    + '<span class="pill">failed: ' + (qMap.failed || 0) + '</span>'
    + '<span class="pill">dispatch/5m: ' + Number(p.dispatched_last_5m || 0) + '</span>';
}

async function loadFeeds() {
  var feeds = await api('/api/db/feeds');
  var tbody = document.querySelector('#feedTable tbody');
  tbody.innerHTML = '';
  feeds.forEach(function(f){
    var tr = document.createElement('tr');
    if (f.url === state.currentFeed) tr.classList.add('selected');
    tr.innerHTML = ''
      + '<td class="url"><div><strong>' + esc(f.source_label || f.url) + '</strong></div><div class="small mono">' + esc(f.url) + '</div></td>'
      + '<td><span class="pill ' + (f.enabled ? (f.health_status === 'broken' ? 'warn':'ok') : 'off') + '">' + esc(f.health_status || (f.enabled ? 'ok' : 'disabled')) + '</span><div class="small">' + esc(f.health_reason || '') + '</div></td>'
      + '<td>' + Number(f.items_count || 0) + '</td>'
      + '<td><div class="row">'
      + '<button class="tiny" onclick="event.stopPropagation();toggleFeed(&quot;' + encodeURIComponent(f.url) + '&quot;,' + (!f.enabled) + ')">' + (f.enabled ? 'Disabilita' : 'Abilita') + '</button>'
      + '<button class="tiny" onclick="event.stopPropagation();forcePollFeed(&quot;' + encodeURIComponent(f.url) + '&quot;)">Poll now</button>'
      + '<button class="tiny" onclick="event.stopPropagation();removeFeed(&quot;' + encodeURIComponent(f.url) + '&quot;)">Rimuovi</button>'
      + '</div></td>';
    tr.onclick = function(){ selectFeed(f.url, f.source_label || f.url, f.items_count || 0); };
    tbody.appendChild(tr);
  });
}

async function selectFeed(url, label, count) {
  state.currentFeed = url;
  await loadFeeds();
  var data = await api('/api/db/feed-items?feed_url=' + encodeURIComponent(url) + '&limit=220');
  var items = data.items || [];
  document.getElementById('selectedFeed').innerHTML = '<strong>' + esc(label) + '</strong> <span class="small">(' + esc(url) + ', items: ' + Number(count || items.length) + ')</span>';
  var tbody = document.querySelector('#itemsTable tbody');
  tbody.innerHTML = '';
  items.forEach(function(it){
    var tr = document.createElement('tr');
    tr.innerHTML = ''
      + '<td title="' + esc(fmtDate(it.created_at)) + '">' + esc(fmtRelative(it.created_at)) + '</td>'
      + '<td><strong>' + esc(it.title || '') + '</strong></td>'
      + '<td><span class="pill">' + esc(it.status || '-') + '</span></td>'
      + '<td><span class="pill">' + esc(it.queue_state || '-') + '</span></td>'
      + '<td class="url"><a href="' + esc(it.url || '#') + '" target="_blank" rel="noreferrer">open</a></td>'
      + '<td><div class="row">'
      + '<button class="tiny" onclick="requeueItem(&quot;' + encodeURIComponent(it.id) + '&quot;)">Requeue</button>'
      + '<button class="tiny" onclick="deleteItem(&quot;' + encodeURIComponent(it.id) + '&quot;)">Delete</button>'
      + '<button class="tiny" onclick="showAnalyzedVersion(&quot;' + encodeURIComponent(it.id) + '&quot;)">View analyzed</button>'
      + '</div></td>';
    tbody.appendChild(tr);
  });
}

async function loadFeedsView() {
  await loadGlobalSummary();
  await loadFeeds();
  if (state.currentFeed) {
    var selected = state.currentFeed;
    state.currentFeed = '';
    await selectFeed(selected, selected, 0);
  }
}

async function addFeed() {
  var input = document.getElementById('newFeedUrl');
  var url = (input.value || '').trim();
  if (!url) return;
  await api('/api/db/feeds', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({url: url}) });
  input.value = '';
  await loadFeedsView();
}

async function toggleFeed(encodedUrl, enabled) {
  var url = decodeURIComponent(encodedUrl);
  await api('/api/db/feeds/toggle', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({url: url, enabled: enabled}) });
  await loadFeeds();
}

async function forcePollFeed(encodedUrl) {
  var url = decodeURIComponent(encodedUrl);
  await api('/api/db/feeds/poll', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({url: url}) });
  await loadGlobalSummary();
}

async function removeFeed(encodedUrl) {
  var url = decodeURIComponent(encodedUrl);
  if (!confirm('Rimuovere feed?\\n' + url)) return;
  await api('/api/db/feeds?url=' + encodeURIComponent(url), { method: 'DELETE' });
  if (state.currentFeed === url) {
    state.currentFeed = '';
    document.querySelector('#itemsTable tbody').innerHTML = '';
    document.getElementById('selectedFeed').textContent = 'Seleziona un feed per vedere gli items memorizzati.';
    document.getElementById('requeuePanel').style.display = 'none';
  }
  await loadFeedsView();
}

function laneColor(name) {
  if (name === 'failed' || name === 'anomalies') return 'off';
  if (name === 'raw_inflight' || name === 'analyzed_inflight') return 'warn';
  return 'ok';
}

async function loadBoard() {
  var limit = Number((document.getElementById('boardLimit').value || '120').trim());
  if (!Number.isFinite(limit) || limit <= 0) limit = 120;
  var data = await api('/api/monitor/board?limit_per_lane=' + encodeURIComponent(String(limit)));
  var lanes = data.lanes || [];
  var root = document.getElementById('boardLanes');
  var total = 0;
  lanes.forEach(function(l){ total += Number(l.count || 0); });
  document.getElementById('boardMeta').innerHTML = '<span class="pill">items visualizzati: ' + total + '</span><span class="pill">limit/lane: ' + Number(data.limit_per_lane || limit) + '</span>';

  root.innerHTML = lanes.map(function(lane){
    var cards = (lane.items || []).map(function(it){
      var errBlock = it.last_error ? ('<div class="small" style="color:#b42318;"><strong>err:</strong> ' + esc(it.last_error) + '</div>') : '';
      return ''
        + '<article class="item-card">'
        + '  <div class="item-title">' + esc(it.title || '(no title)') + '</div>'
        + '  <div class="item-meta">'
        + '    <div><span class="pill">' + esc(it.status || '-') + '</span> <span class="pill">' + esc(it.queue_state || '-') + '</span></div>'
        + '    <div>feed: <span class="mono">' + esc(it.source_label || '-') + '</span></div>'
        + '    <div>id: <span class="mono">' + esc(it.id || '') + '</span></div>'
        + '    <div>created: ' + esc(fmtDate(it.created_at)) + '</div>'
        + '  </div>'
        + errBlock
        + '  <div class="item-actions">'
        + '    <a href="' + esc(it.url || '#') + '" target="_blank" rel="noreferrer"><button class="tiny">Open</button></a>'
        + '    <button class="tiny" onclick="requeueItem(&quot;' + encodeURIComponent(it.id) + '&quot;)">Requeue</button>'
        + '    <button class="tiny" onclick="deleteItem(&quot;' + encodeURIComponent(it.id) + '&quot;)">Delete</button>'
        + '    <button class="tiny" onclick="showAnalyzedVersion(&quot;' + encodeURIComponent(it.id) + '&quot;)">View analyzed</button>'
        + '  </div>'
        + '</article>';
    }).join('');
    if (!cards) cards = '<div class="small muted">Nessun item in questa lane.</div>';
    return ''
      + '<section class="lane">'
      + '  <div class="lane-head">'
      + '    <div class="lane-title">' + esc(lane.title || lane.name) + '</div>'
      + '    <span class="pill ' + laneColor(lane.name) + '">' + Number(lane.count || 0) + '</span>'
      + '  </div>'
      + '  <div class="lane-items">' + cards + '</div>'
      + '</section>';
  }).join('');
}

function startBoardAutoRefresh() {
  stopBoardAutoRefresh();
  if (!state.boardAutoRefresh) return;
  state.boardTimer = setInterval(function(){
    if (state.currentView === 'board') {
      loadBoard().then(setLastRefresh).catch(showError);
      loadGlobalSummary().catch(showError);
    }
  }, 5000);
}

function stopBoardAutoRefresh() {
  if (state.boardTimer) {
    clearInterval(state.boardTimer);
    state.boardTimer = null;
  }
}

function toggleBoardAutoRefresh() {
  state.boardAutoRefresh = !state.boardAutoRefresh;
  document.getElementById('boardAutoBtn').textContent = 'Auto-refresh: ' + (state.boardAutoRefresh ? 'ON' : 'OFF');
  if (state.boardAutoRefresh) startBoardAutoRefresh(); else stopBoardAutoRefresh();
}

function flowReset() {
  ['stepQueued','stepAnalyzed','stepDispatched'].forEach(function(id){
    var el = document.getElementById(id);
    el.classList.remove('active','done','fail');
  });
}

function renderRequeueState(itemID, status, err, updatedAt, active) {
  var panel = document.getElementById('requeuePanel');
  var label = document.getElementById('requeueItemLabel');
  var pill = document.getElementById('requeueStatusPill');
  var text = document.getElementById('requeueStatusText');
  panel.style.display = 'block';
  label.textContent = itemID || '';
  flowReset();
  var sQ = document.getElementById('stepQueued');
  var sA = document.getElementById('stepAnalyzed');
  var sD = document.getElementById('stepDispatched');

  if (status === 'NEW') {
    sQ.classList.add('active');
    pill.className = 'pill warn';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'In coda raw. Ultimo update: ' + esc(fmtDate(updatedAt));
  } else if (status === 'ANALYZED') {
    sQ.classList.add('done');
    sA.classList.add('active');
    pill.className = 'pill ok';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'Analizzato e in coda dispatch. Ultimo update: ' + esc(fmtDate(updatedAt));
  } else if (status === 'DISPATCHED') {
    sQ.classList.add('done'); sA.classList.add('done'); sD.classList.add('done');
    pill.className = 'pill ok';
    text.textContent = 'Dispatch completato alle ' + fmtDate(updatedAt);
  } else if (status === 'FAILED') {
    sQ.classList.add('done'); sA.classList.add('fail');
    pill.className = 'pill off';
    text.textContent = 'Errore: ' + (err || 'unknown');
  } else {
    sQ.classList.add('active');
    pill.className = 'pill warn';
    text.innerHTML = (active ? '<span class="spinner"></span>' : '') + 'Stato corrente: ' + esc(status || 'sconosciuto');
  }
  pill.textContent = status || 'PENDING';
  if (!active && state.requeuePollTimer) {
    clearInterval(state.requeuePollTimer);
    state.requeuePollTimer = null;
  }
}

async function requeueItem(encodedId) {
  var id = decodeURIComponent(encodedId);
  if (state.requeuePollTimer) {
    clearInterval(state.requeuePollTimer);
    state.requeuePollTimer = null;
  }
  renderRequeueState(id, 'NEW', '', new Date().toISOString(), true);
  try {
    await api('/api/db/items/requeue', { method: 'POST', headers: {'Content-Type': 'application/json'}, body: JSON.stringify({id: id}) });
    state.requeuePollTimer = setInterval(async function(){
      var st = await api('/api/db/items/status?id=' + encodeURIComponent(id));
      var active = st.status !== 'DISPATCHED' && st.status !== 'FAILED';
      renderRequeueState(id, st.status, st.last_error || '', st.updated_at, active);
    }, 1500);
  } catch (err) {
    renderRequeueState(id, 'FAILED', err.message || String(err), new Date().toISOString(), false);
  } finally {
    await reloadCurrentView();
  }
}

async function deleteItem(encodedId) {
  var id = decodeURIComponent(encodedId);
  if (!confirm('Eliminare item ' + id + '?')) return;
  await api('/api/db/items?id=' + encodeURIComponent(id), { method: 'DELETE' });
  await reloadCurrentView();
}

async function showAnalyzedVersion(encodedId) {
  var id = decodeURIComponent(encodedId);
  var data = await api('/api/db/items/analyzed?id=' + encodeURIComponent(id));
  showJSONModal(id, data.analyzed_payload_json || '');
}

function syntaxHighlightJSON(v) {
  var s = JSON.stringify(v, null, 2);
  return esc(s).replace(/(&quot;[^&]*&quot;)(\s*:)?|\b(true|false|null)\b|-?\d+(?:\.\d+)?(?:[eE][+\-]?\d+)?/g, function(m, s1, s2){
    if (s1) return '<span class="j-key">' + s1 + '</span>' + (s2 || '');
    if (/^(true|false)$/.test(m)) return '<span class="j-bool">' + m + '</span>';
    if (/^null$/.test(m)) return '<span class="j-null">' + m + '</span>';
    return '<span class="j-num">' + m + '</span>';
  });
}

function showJSONModal(id, rawPayload) {
  var bd = document.getElementById('jsonModalBackdrop');
  var body = document.getElementById('jsonModalBody');
  document.getElementById('jsonModalID').textContent = id || '';
  var parsed = null;
  try { parsed = JSON.parse(rawPayload || '{}'); } catch (_) {}
  body.innerHTML = parsed ? syntaxHighlightJSON(parsed) : esc(rawPayload || '');
  bd.style.display = 'flex';
}

function closeJSONModal(ev) {
  if (ev && ev.target && ev.target.id !== 'jsonModalBackdrop') return;
  document.getElementById('jsonModalBackdrop').style.display = 'none';
}

function showError(err) {
  alert(err && err.message ? err.message : String(err));
}

async function reloadCurrentView() {
  if (state.currentView === 'board') {
    await loadGlobalSummary();
    await loadBoard();
  } else {
    await loadFeedsView();
  }
  setLastRefresh();
}

(async function init(){
  try {
    renderViewMenu();
    await loadFeedsView();
    setLastRefresh();
  } catch (err) {
    showError(err);
  }
})();
</script>
</body>
</html>`
