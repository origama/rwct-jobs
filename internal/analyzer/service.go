package analyzer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"

	"rwct-agent/pkg/events"
	"rwct-agent/pkg/retry"
	"rwct-agent/pkg/telemetry"
)

var allowedJobCategories = []string{
	"Programmazione",
	"Devops & Sysadmin",
	"Customer Support",
	"Marketing",
	"Design",
	"Business",
	"Copywriting",
	"Jobs",
	"Altri ruoli",
}

var normalizedJobCategories = map[string]string{
	"programmazione":      "Programmazione",
	"devops & sysadmin":   "Devops & Sysadmin",
	"devops and sysadmin": "Devops & Sysadmin",
	"devops/sysadmin":     "Devops & Sysadmin",
	"customer support":    "Customer Support",
	"marketing":           "Marketing",
	"design":              "Design",
	"business":            "Business",
	"copywriting":         "Copywriting",
	"jobs":                "Jobs",
	"altri ruoli":         "Altri ruoli",
	"altri ruoli.":        "Altri ruoli",
	"other roles":         "Altri ruoli",
}

type Config struct {
	DBPath                 string
	QueuePollInterval      time.Duration
	LeaseDuration          time.Duration
	LLMEndpoint            string
	LLMModel               string
	LLMTimeout             time.Duration
	LLMTimeoutMax          time.Duration
	LLMTimeoutPer1KChars   time.Duration
	LLMTimeoutPer256Tokens time.Duration
	LLMMaxTokens           int
	LLMThinking            bool
	LLMStrictJSON          bool
	PromptTemplate         string
	CompactPromptTemplate  string
	SourceExtractor        string
	SourceMinChars         int
	ScraplingEndpoint      string
	ScraplingTimeout       time.Duration
	ScraplingMaxChars      int
	MaxConcurrency         int
	MaxJobsPerMin          int
	MaxDeliveryAttempts    int
	RetryAttempts          int
	RetryBaseDelay         time.Duration
}

const (
	sourceExtractorOff       = "off"
	sourceExtractorBasic     = "basic"
	sourceExtractorScrapling = "scrapling"
	sourceExtractorHybrid    = "hybrid"
)

var (
	reScriptTags = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`)
	reStyleTags  = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`)
	reHTMLTags   = regexp.MustCompile(`(?s)<[^>]+>`)
)

type Service struct {
	cfg      Config
	db       *sql.DB
	http     *http.Client
	limiter  *rate.Limiter
	workerID string

	tracer trace.Tracer

	analyzeDurationSec   otelmetric.Float64Histogram
	llmDurationSec       otelmetric.Float64Histogram
	genAIOperationSec    otelmetric.Float64Histogram
	genAITokenUsage      otelmetric.Int64Histogram
	jobsProcessed        otelmetric.Int64Counter
	jobsFailed           otelmetric.Int64Counter
	sourceScrapeFailures otelmetric.Int64Counter
}

func New(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = ":memory:"
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 1
	}
	if cfg.MaxConcurrency != 1 {
		slog.Warn("max concurrency overridden for strict sequential mode", "requested", cfg.MaxConcurrency, "effective", 1)
		cfg.MaxConcurrency = 1
	}
	if cfg.MaxJobsPerMin < 1 {
		cfg.MaxJobsPerMin = 10
	}
	if cfg.MaxDeliveryAttempts < 1 {
		cfg.MaxDeliveryAttempts = 3
	}
	if cfg.QueuePollInterval <= 0 {
		cfg.QueuePollInterval = 1200 * time.Millisecond
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 2 * time.Minute
	}
	cfg.SourceExtractor = normalizeSourceExtractor(cfg.SourceExtractor)
	if cfg.SourceMinChars < 1 {
		cfg.SourceMinChars = 220
	}
	if strings.TrimSpace(cfg.ScraplingEndpoint) == "" {
		cfg.ScraplingEndpoint = "http://scrapling-sidecar:8088"
	}
	if cfg.ScraplingTimeout <= 0 {
		cfg.ScraplingTimeout = 8 * time.Second
	}
	if cfg.ScraplingMaxChars < 1 {
		cfg.ScraplingMaxChars = 2500
	}
	if cfg.LLMTimeout <= 0 {
		cfg.LLMTimeout = 20 * time.Second
	}
	if cfg.LLMTimeoutMax <= 0 {
		cfg.LLMTimeoutMax = 10 * time.Minute
	}
	if cfg.LLMTimeoutMax < cfg.LLMTimeout {
		cfg.LLMTimeoutMax = cfg.LLMTimeout
	}
	if cfg.LLMTimeoutPer1KChars <= 0 {
		cfg.LLMTimeoutPer1KChars = 25 * time.Second
	}
	if cfg.LLMTimeoutPer256Tokens <= 0 {
		cfg.LLMTimeoutPer256Tokens = 20 * time.Second
	}
	db, err := sql.Open("sqlite", sqliteDSN(cfg.DBPath))
	if err != nil {
		return nil, err
	}
	if err := initStateDB(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Service{
		cfg: cfg,
		db:  db,
		http: &http.Client{
			Transport: telemetry.NewHTTPTransport(http.DefaultTransport),
		},
		limiter:  rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.MaxJobsPerMin)), 1),
		workerID: fmt.Sprintf("%s-%d", safeHostname(), time.Now().UTC().UnixNano()),
		tracer:   otel.Tracer("rwct-agent/job-analyzer"),
		analyzeDurationSec: mustFloat64Histogram("rwct_analyzer_duration_seconds",
			"Duration of analyzer jobs"),
		llmDurationSec: mustFloat64Histogram("rwct_analyzer_llm_duration_seconds",
			"Duration of LLM requests from analyzer"),
		genAIOperationSec: mustFloat64HistogramWithUnit("gen_ai.client.operation.duration",
			"Duration of GenAI client operations", "s"),
		genAITokenUsage: mustInt64HistogramWithUnit("gen_ai.client.token.usage",
			"Number of input and output tokens used per GenAI client operation", "{token}"),
		jobsProcessed: mustInt64Counter("rwct_analyzer_jobs_total",
			"Number of analyzer jobs processed"),
		jobsFailed: mustInt64Counter("rwct_analyzer_job_failures_total",
			"Number of analyzer jobs that failed"),
		sourceScrapeFailures: mustInt64Counter("rwct_analyzer_source_scrape_failures_total",
			"Number of source-page scraping failures"),
	}, nil
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func analyzerMeter() otelmetric.Meter {
	return otel.Meter("rwct-agent/job-analyzer")
}

func mustFloat64Histogram(name, description string) otelmetric.Float64Histogram {
	h, _ := analyzerMeter().Float64Histogram(name, otelmetric.WithDescription(description))
	return h
}

func mustFloat64HistogramWithUnit(name, description, unit string) otelmetric.Float64Histogram {
	h, _ := analyzerMeter().Float64Histogram(name,
		otelmetric.WithDescription(description),
		otelmetric.WithUnit(unit))
	return h
}

func mustInt64HistogramWithUnit(name, description, unit string) otelmetric.Int64Histogram {
	h, _ := analyzerMeter().Int64Histogram(name,
		otelmetric.WithDescription(description),
		otelmetric.WithUnit(unit))
	return h
}

func mustInt64Counter(name, description string) otelmetric.Int64Counter {
	c, _ := analyzerMeter().Int64Counter(name, otelmetric.WithDescription(description))
	return c
}

func (s *Service) tracerForUse() trace.Tracer {
	if s.tracer != nil {
		return s.tracer
	}
	return otel.Tracer("rwct-agent/job-analyzer")
}

func (s *Service) recordAnalyzeDuration(ctx context.Context, seconds float64, opts ...otelmetric.RecordOption) {
	if s.analyzeDurationSec != nil {
		s.analyzeDurationSec.Record(ctx, seconds, opts...)
	}
}

func (s *Service) recordLLMDuration(ctx context.Context, seconds float64, opts ...otelmetric.RecordOption) {
	if s.llmDurationSec != nil {
		s.llmDurationSec.Record(ctx, seconds, opts...)
	}
}

func (s *Service) recordGenAIOperationDuration(ctx context.Context, seconds float64, model string) {
	if s.genAIOperationSec == nil {
		return
	}
	s.genAIOperationSec.Record(ctx, seconds, otelmetric.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", "llama_cpp"),
		attribute.String("gen_ai.request.model", model),
	))
}

func (s *Service) recordGenAITokenUsage(ctx context.Context, tokenType string, tokens int64, model string) {
	if s.genAITokenUsage == nil || tokens <= 0 {
		return
	}
	s.genAITokenUsage.Record(ctx, tokens, otelmetric.WithAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.provider.name", "llama_cpp"),
		attribute.String("gen_ai.request.model", model),
		attribute.String("gen_ai.token.type", tokenType),
	))
}

func (s *Service) addProcessed(ctx context.Context, opts ...otelmetric.AddOption) {
	if s.jobsProcessed != nil {
		s.jobsProcessed.Add(ctx, 1, opts...)
	}
}

func (s *Service) addFailed(ctx context.Context, opts ...otelmetric.AddOption) {
	if s.jobsFailed != nil {
		s.jobsFailed.Add(ctx, 1, opts...)
	}
}

func (s *Service) addSourceFailure(ctx context.Context, opts ...otelmetric.AddOption) {
	if s.sourceScrapeFailures != nil {
		s.sourceScrapeFailures.Add(ctx, 1, opts...)
	}
}

func initStateDB(db *sql.DB) error {
	schema := `
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
	status TEXT NOT NULL,
	analyzed_at DATETIME,
	analyzed_payload_json TEXT,
	dispatched_at DATETIME,
	last_error TEXT,
	created_at DATETIME,
	updated_at DATETIME NOT NULL
);

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
);`
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
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	slog.Info("job-analyzer started", "queue", "analyzer_queue", "worker_id", s.workerID, "model", s.cfg.LLMModel)
	defer s.db.Close()
	s.upsertRuntimeSnapshot()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		itemID, payload, ok, err := s.claimNextRawJob(ctx)
		if err != nil {
			slog.Error("claim raw job failed", "worker_id", s.workerID, "err", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.cfg.QueuePollInterval):
				continue
			}
		}
		if !ok {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(s.cfg.QueuePollInterval):
				continue
			}
		}

		if err := s.limiter.Wait(ctx); err != nil {
			_ = s.releaseClaim(itemID, fmt.Sprintf("rate limiter wait failed: %v", err))
			return err
		}
		if err := s.handleClaimedMessage(ctx, itemID, payload); err != nil {
			slog.Error("analyzer message failed", "item_id", itemID, "worker_id", s.workerID, "err", err)
		}
	}
}

func (s *Service) handleClaimedMessage(ctx context.Context, itemID string, payload []byte) error {
	started := time.Now()
	var in events.RawJobItem
	if err := json.Unmarshal(payload, &in); err != nil {
		_ = s.releaseClaim(itemID, fmt.Sprintf("invalid_raw_payload: %v", err))
		s.addFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "invalid_raw_payload")))
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		in.ID = itemID
	}
	ctx = telemetry.ExtractTraceContext(ctx, in.TraceParent, in.TraceState)
	ctx, span := s.tracerForUse().Start(ctx, "analyzer.handle_item",
		trace.WithAttributes(
			attribute.String("item.id", in.ID),
			attribute.String("item.url", in.URL),
		))
	defer span.End()
	defer s.recordAnalyzeDuration(ctx, time.Since(started).Seconds())

	if already, analyzedPayload, err := s.loadStoredAnalyzed(in.ID); err == nil && already {
		if err := s.enqueueDispatchJob(in.ID, []byte(analyzedPayload)); err != nil {
			_ = s.releaseClaim(itemID, fmt.Sprintf("enqueue_stored_dispatch_failed: %v", err))
			span.RecordError(err)
			s.addFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "enqueue_stored_dispatch_failed")))
			return err
		}
		s.addProcessed(ctx, otelmetric.WithAttributes(attribute.String("status", "cached")))
		return s.completeClaim(itemID)
	}

	var analyzed events.AnalyzedJob
	err := retry.Exponential(ctx, s.cfg.RetryAttempts, s.cfg.RetryBaseDelay, func(attempt int) error {
		out, e := s.analyze(ctx, in)
		if e != nil {
			_ = s.markItemStatus(in.ID, events.ItemStatusFailed, e.Error(), nil)
			s.publishError(in.ID, attempt, "llm_call_failed", e)
			span.RecordError(e)
			return e
		}
		analyzed = out
		return nil
	})
	if err != nil {
		slog.Error("analysis failed after retries", "item_id", in.ID, "err", err)
		s.addFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "analysis_failed")))
		if s.shouldQuarantineClaim(itemID, err) {
			reason := fmt.Sprintf("poison_item: analysis_failed: %v", err)
			_ = s.markItemStatus(in.ID, events.ItemStatusFailed, reason, nil)
			_ = s.failClaim(itemID, reason)
			slog.Warn("claim quarantined after repeated/non-retryable failures", "item_id", in.ID, "worker_id", s.workerID)
			return err
		}
		_ = s.releaseClaim(itemID, fmt.Sprintf("analysis_failed: %v", err))
		return err
	}

	_ = s.markItemStatus(in.ID, events.ItemStatusAnalyzed, "", &analyzed)
	body, _ := json.Marshal(analyzed)
	if err := s.enqueueDispatchJob(in.ID, body); err != nil {
		_ = s.releaseClaim(itemID, fmt.Sprintf("enqueue_dispatch_failed: %v", err))
		span.RecordError(err)
		s.addFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "enqueue_dispatch_failed")))
		return err
	}
	s.addProcessed(ctx, otelmetric.WithAttributes(attribute.String("status", "success")))
	return s.completeClaim(itemID)
}

func (s *Service) enqueueDispatchJob(itemID string, payload []byte) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`INSERT INTO dispatch_queue(item_id, payload_json, state, enqueued_at, updated_at)
VALUES(?, ?, 'QUEUED', ?, ?)
ON CONFLICT(item_id) DO UPDATE SET
 payload_json=excluded.payload_json,
 state='QUEUED',
 lease_owner=NULL,
 lease_until=NULL,
 last_error='',
 updated_at=excluded.updated_at`, strings.TrimSpace(itemID), string(payload), now, now)
	if err != nil {
		return err
	}
	_, _ = s.db.Exec(
		`UPDATE rss_items
SET analyzed_enqueued_at = COALESCE(?, analyzed_enqueued_at),
    analyzed_enqueue_count = COALESCE(analyzed_enqueue_count, 0) + 1,
    updated_at = ?
WHERE id = ?`,
		now, now, strings.TrimSpace(itemID),
	)
	return nil
}

func (s *Service) claimNextRawJob(ctx context.Context) (string, []byte, bool, error) {
	now := time.Now().UTC()
	leaseUntil := now.Add(s.cfg.LeaseDuration)
	row := s.db.QueryRowContext(ctx, `UPDATE analyzer_queue
SET state='LEASED',
    lease_owner=?,
    lease_until=?,
    delivery_count=COALESCE(delivery_count,0)+1,
    last_error='',
    updated_at=?
WHERE item_id = (
  SELECT item_id FROM analyzer_queue
  WHERE state='QUEUED' OR (state='LEASED' AND lease_until IS NOT NULL AND lease_until <= ?)
  ORDER BY enqueued_at
  LIMIT 1
)
RETURNING item_id, payload_json`, s.workerID, leaseUntil, now, now)

	var itemID string
	var payloadJSON string
	if err := row.Scan(&itemID, &payloadJSON); err != nil {
		if err == sql.ErrNoRows {
			return "", nil, false, nil
		}
		return "", nil, false, err
	}
	return itemID, []byte(payloadJSON), true, nil
}

func (s *Service) completeClaim(itemID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE analyzer_queue
SET state='DONE',
    lease_owner=NULL,
    lease_until=NULL,
    done_at=COALESCE(done_at, ?),
    last_error='',
    updated_at=?
WHERE item_id = ? AND lease_owner = ?`, now, now, strings.TrimSpace(itemID), s.workerID)
	return err
}

func (s *Service) releaseClaim(itemID, reason string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE analyzer_queue
SET state='QUEUED',
    lease_owner=NULL,
    lease_until=NULL,
    last_error=?,
    updated_at=?
WHERE item_id = ? AND lease_owner = ?`, strings.TrimSpace(reason), now, strings.TrimSpace(itemID), s.workerID)
	return err
}

func (s *Service) failClaim(itemID, reason string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE analyzer_queue
SET state='DONE',
    lease_owner=NULL,
    lease_until=NULL,
    done_at=COALESCE(done_at, ?),
    last_error=?,
    updated_at=?
WHERE item_id = ? AND lease_owner = ?`, now, strings.TrimSpace(reason), now, strings.TrimSpace(itemID), s.workerID)
	return err
}

func (s *Service) loadStoredAnalyzed(itemID string) (bool, string, error) {
	var payload string
	err := s.db.QueryRow(`SELECT COALESCE(analyzed_payload_json,'') FROM rss_items WHERE id = ?`, strings.TrimSpace(itemID)).Scan(&payload)
	if err == sql.ErrNoRows {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return false, "", nil
	}
	return true, payload, nil
}

func safeHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "analyzer"
	}
	h = strings.TrimSpace(h)
	if h == "" {
		return "analyzer"
	}
	return h
}

func (s *Service) markItemStatus(itemID, status, lastError string, analyzed *events.AnalyzedJob) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	now := time.Now().UTC()
	var analyzedAt any
	var analyzedPayload any
	switch status {
	case events.ItemStatusAnalyzed:
		analyzedAt = now
		if analyzed != nil {
			if b, err := json.Marshal(analyzed); err == nil {
				analyzedPayload = string(b)
			}
		}
	}
	_, err := s.db.Exec(
		`UPDATE rss_items
		SET status = ?,
		    analyzed_at = COALESCE(?, analyzed_at),
		    analyzed_payload_json = COALESCE(?, analyzed_payload_json),
		    last_error = ?,
		    updated_at = ?
		WHERE id = ? AND status <> ?`,
		status, analyzedAt, analyzedPayload, strings.TrimSpace(lastError), now, itemID, events.ItemStatusDispatched,
	)
	return err
}

func (s *Service) publishError(itemID string, attempt int, code string, err error) {
	slog.Warn("analyzer processing error", "item_id", itemID, "attempt", attempt, "code", code, "err", err)
}

func (s *Service) analyze(ctx context.Context, in events.RawJobItem) (events.AnalyzedJob, error) {
	ctx, span := s.tracerForUse().Start(ctx, "analyzer.analyze",
		trace.WithAttributes(
			attribute.String("item.id", in.ID),
			attribute.String("source.extractor", s.cfg.SourceExtractor),
		))
	defer span.End()

	sourcePageText := ""
	if body, err := s.fetchSourcePageExcerpt(ctx, in.URL); err == nil {
		sourcePageText = body
	} else if strings.TrimSpace(in.URL) != "" {
		span.RecordError(err)
		s.addSourceFailure(ctx, otelmetric.WithAttributes(
			attribute.String("extractor", s.cfg.SourceExtractor),
		))
		slog.Warn(
			"source page scraping failed; continuing with feed payload only",
			"item_id", in.ID,
			"url", in.URL,
			"strategy", s.cfg.SourceExtractor,
			"err", err,
		)
	}

	content, err := s.callLLM(ctx, s.buildPromptForLLM(in, sourcePageText), s.cfg.LLMMaxTokens)
	if err != nil {
		span.RecordError(err)
		return events.AnalyzedJob{}, err
	}
	out, parseErr := parseAnalyzedJob(content, in)
	if parseErr == nil {
		traceParent, traceState := telemetry.InjectTraceContext(ctx)
		out.TraceParent = traceParent
		out.TraceState = traceState
		return out, nil
	}

	// Fallback for truncated outputs: ask for compact JSON and allow more tokens.
	if isTruncatedJSONError(parseErr) {
		fallbackTokens := s.cfg.LLMMaxTokens * 2
		if fallbackTokens < 512 {
			fallbackTokens = 512
		}
		if fallbackTokens > 1024 {
			fallbackTokens = 1024
		}
		slog.Warn("llm returned truncated json, retrying compact mode", "item_id", in.ID, "max_tokens", fallbackTokens)
		content2, err2 := s.callLLM(ctx, s.buildCompactPromptForLLM(in, sourcePageText), fallbackTokens)
		if err2 != nil {
			span.RecordError(err2)
			return events.AnalyzedJob{}, err2
		}
		out2, parseErr2 := parseAnalyzedJob(content2, in)
		if parseErr2 == nil {
			traceParent, traceState := telemetry.InjectTraceContext(ctx)
			out2.TraceParent = traceParent
			out2.TraceState = traceState
			return out2, nil
		}
		span.RecordError(parseErr2)
		return events.AnalyzedJob{}, parseErr2
	}
	span.RecordError(parseErr)
	return events.AnalyzedJob{}, parseErr
}

func buildPrompt(in events.RawJobItem, sourcePageText string) string {
	description := strings.TrimSpace(in.Description)
	if len(description) > 1800 {
		description = description[:1800]
	}
	sourcePageText = strings.TrimSpace(sourcePageText)
	if sourcePageText == "" {
		sourcePageText = "N/D"
	}
	return fmt.Sprintf(`Analizza il seguente annuncio lavoro e restituisci SOLO JSON valido con i campi:
job_category, role, company, seniority, location, remote_type, tech_stack (array), tags (array), contract_type, salary, language, summary_it, confidence.
Lingua output: italiano.
Regola anti-allucinazioni (obbligatoria): usa solo informazioni esplicitamente presenti in titolo/descrizione/link forniti.
NON inferire da conoscenza esterna e NON inventare valori mancanti.
Se un campo non è chiaramente presente usa stringa vuota (o array vuoto).
Campi da valorizzare con massima priorita' (se presenti): seniority, location, contract_type, salary, tech_stack.
Mantieni tech_stack breve: massimo 8 tecnologie reali, senza inventare valori.
tags: array di 3-6 tag distintivi, minuscoli, senza #, max 24 caratteri per tag (es: go, kubernetes, remote, fintech, full-time).
summary_it: una sola frase, massimo 220 caratteri.
Per job_category usa SOLO una di queste categorie esatte:
%s
Se non è classificabile, usa esattamente: Altri ruoli.

Titolo: %s
URL: %s
Descrizione: %s
Contenuto pagina sorgente (estratto): %s
Link estratti: %s
Immagini estratte: %s`, strings.Join(allowedJobCategories, ", "), in.Title, in.URL, description, sourcePageText, strings.Join(in.Links, ", "), strings.Join(in.ImageURLs, ", "))
}

func buildCompactPrompt(in events.RawJobItem, sourcePageText string) string {
	description := strings.TrimSpace(in.Description)
	if len(description) > 1200 {
		description = description[:1200]
	}
	sourcePageText = clampString(strings.TrimSpace(sourcePageText), 1600)
	if sourcePageText == "" {
		sourcePageText = "N/D"
	}
	return fmt.Sprintf(`Restituisci SOLO 1 riga JSON valida (nessun markdown, nessun testo extra) con questi campi:
job_category,role,company,seniority,location,remote_type,tech_stack,tags,contract_type,salary,language,summary_it,confidence
Vincoli:
- job_category deve essere una di: %s
- usa SOLO informazioni esplicitamente presenti nel testo fornito
- non inventare: se non chiaro/menzionato usa stringa vuota o array vuoto
- tech_stack massimo 8 elementi
- tags: 3-6 valori lowercase, senza #, max 24 caratteri ciascuno
- summary_it massimo 220 caratteri

Titolo: %s
URL: %s
Descrizione: %s
Contenuto pagina sorgente (estratto): %s`, strings.Join(allowedJobCategories, ", "), in.Title, in.URL, description, sourcePageText)
}

func (s *Service) sourceExcerptMaxChars() int {
	if s.cfg.ScraplingMaxChars > 0 {
		return s.cfg.ScraplingMaxChars
	}
	return 2500
}

type promptTemplateData struct {
	AllowedCategories string
	Title             string
	URL               string
	Description       string
	SourcePageText    string
	LinksCSV          string
	ImagesCSV         string
}

func (s *Service) buildPromptForLLM(in events.RawJobItem, sourcePageText string) string {
	if strings.TrimSpace(s.cfg.PromptTemplate) == "" {
		return buildPrompt(in, sourcePageText)
	}
	rendered, err := renderPromptTemplate(s.cfg.PromptTemplate, in, sourcePageText, 1800, s.sourceExcerptMaxChars())
	if err != nil {
		slog.Warn("invalid analyzer prompt template; fallback to default", "err", err)
		return buildPrompt(in, sourcePageText)
	}
	return rendered
}

func (s *Service) buildCompactPromptForLLM(in events.RawJobItem, sourcePageText string) string {
	if strings.TrimSpace(s.cfg.CompactPromptTemplate) == "" {
		return buildCompactPrompt(in, sourcePageText)
	}
	rendered, err := renderPromptTemplate(s.cfg.CompactPromptTemplate, in, sourcePageText, 1200, 1600)
	if err != nil {
		slog.Warn("invalid analyzer compact prompt template; fallback to default", "err", err)
		return buildCompactPrompt(in, sourcePageText)
	}
	return rendered
}

func renderPromptTemplate(raw string, in events.RawJobItem, sourcePageText string, descMax, sourceMax int) (string, error) {
	description := strings.TrimSpace(in.Description)
	if len(description) > descMax {
		description = description[:descMax]
	}
	sourcePageText = clampString(strings.TrimSpace(sourcePageText), sourceMax)
	if sourcePageText == "" {
		sourcePageText = "N/D"
	}
	data := promptTemplateData{
		AllowedCategories: strings.Join(allowedJobCategories, ", "),
		Title:             in.Title,
		URL:               in.URL,
		Description:       description,
		SourcePageText:    sourcePageText,
		LinksCSV:          strings.Join(in.Links, ", "),
		ImagesCSV:         strings.Join(in.ImageURLs, ", "),
	}
	tpl, err := template.New("analyzer-prompt").Option("missingkey=error").Parse(raw)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := tpl.Execute(&out, data); err != nil {
		return "", err
	}
	rendered := strings.TrimSpace(out.String())
	if rendered == "" {
		return "", errors.New("rendered prompt is empty")
	}
	return rendered, nil
}

func stripMarkdownCodeFence(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "```json")
	v = strings.TrimPrefix(v, "```")
	v = strings.TrimSuffix(v, "```")
	return strings.TrimSpace(v)
}

func analyzedJobJSONSchema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required": []string{
			"job_category", "role", "company", "seniority", "location", "remote_type",
			"tech_stack", "tags", "contract_type", "salary", "language", "summary_it", "confidence",
		},
		"properties": map[string]any{
			"job_category": map[string]any{
				"type": "string",
				"enum": allowedJobCategories,
			},
			"role":          map[string]any{"type": "string"},
			"company":       map[string]any{"type": "string"},
			"seniority":     map[string]any{"type": "string"},
			"location":      map[string]any{"type": "string"},
			"remote_type":   map[string]any{"type": "string"},
			"contract_type": map[string]any{"type": "string"},
			"salary":        map[string]any{"type": "string"},
			"language":      map[string]any{"type": "string"},
			"summary_it": map[string]any{
				"type":      "string",
				"maxLength": 220,
			},
			"tech_stack": map[string]any{
				"type":        "array",
				"minItems":    0,
				"maxItems":    8,
				"uniqueItems": true,
				"items": map[string]any{
					"type":      "string",
					"maxLength": 32,
				},
			},
			"tags": map[string]any{
				"type":        "array",
				"minItems":    0,
				"maxItems":    6,
				"uniqueItems": true,
				"items": map[string]any{
					"type":      "string",
					"maxLength": 24,
				},
			},
			"confidence": map[string]any{
				"type":    "number",
				"minimum": 0,
				"maximum": 1,
			},
		},
	}
}

func (s *Service) callLLM(ctx context.Context, prompt string, maxTokens int) (string, error) {
	ctx, span := s.tracerForUse().Start(ctx, "analyzer.call_llm",
		trace.WithAttributes(
			attribute.Int("llm.max_tokens", maxTokens),
			attribute.Int("prompt.chars", len(prompt)),
			attribute.String("llm.model", s.cfg.LLMModel),
		))
	defer span.End()
	started := time.Now()
	defer s.recordLLMDuration(ctx, time.Since(started).Seconds(),
		otelmetric.WithAttributes(attribute.String("llm.model", s.cfg.LLMModel)))
	defer s.recordGenAIOperationDuration(ctx, time.Since(started).Seconds(), s.cfg.LLMModel)

	timeout := s.computeLLMTimeout(prompt, maxTokens)
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	reqBody := map[string]any{
		"model": s.cfg.LLMModel,
		"messages": []map[string]string{
			{"role": "system", "content": "Sei un analizzatore annunci lavoro. Rispondi solo JSON valido."},
			{"role": "user", "content": prompt},
		},
		"temperature": 0.1,
		"max_tokens":  maxTokens,
	}
	if s.cfg.LLMThinking {
		reqBody["enable_thinking"] = true
	}
	if s.cfg.LLMStrictJSON {
		schema := analyzedJobJSONSchema()
		reqBody["response_format"] = map[string]any{
			"type":   "json_schema",
			"schema": schema,
		}
		// Fallback for llama.cpp implementations that map json_schema directly to grammar-based sampling.
		reqBody["json_schema"] = schema
	}
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, strings.TrimRight(s.cfg.LLMEndpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		span.RecordError(err)
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		span.RecordError(err)
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		err := fmt.Errorf("llm status: %d", resp.StatusCode)
		span.RecordError(err)
		span.SetAttributes(attribute.Int("http.status_code", resp.StatusCode))
		return "", err
	}

	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		span.RecordError(err)
		return "", err
	}
	if len(r.Choices) == 0 {
		err := errors.New("empty llm response")
		span.RecordError(err)
		return "", err
	}
	s.recordGenAITokenUsage(ctx, "input", r.Usage.PromptTokens, s.cfg.LLMModel)
	s.recordGenAITokenUsage(ctx, "output", r.Usage.CompletionTokens, s.cfg.LLMModel)
	return stripMarkdownCodeFence(strings.TrimSpace(r.Choices[0].Message.Content)), nil
}

func (s *Service) computeLLMTimeout(prompt string, maxTokens int) time.Duration {
	base := s.cfg.LLMTimeout
	if base <= 0 {
		base = 20 * time.Second
	}
	maxTimeout := s.cfg.LLMTimeoutMax
	if maxTimeout <= 0 {
		maxTimeout = 10 * time.Minute
	}
	if maxTimeout < base {
		maxTimeout = base
	}
	per1k := s.cfg.LLMTimeoutPer1KChars
	if per1k <= 0 {
		per1k = 25 * time.Second
	}
	per256 := s.cfg.LLMTimeoutPer256Tokens
	if per256 <= 0 {
		per256 = 20 * time.Second
	}

	timeout := base
	chars := len(strings.TrimSpace(prompt))
	if chars > 0 {
		steps := (chars + 999) / 1000
		timeout += time.Duration(steps) * per1k
	}
	if maxTokens > 0 {
		steps := (maxTokens + 255) / 256
		timeout += time.Duration(steps) * per256
	}
	if timeout > maxTimeout {
		timeout = maxTimeout
	}
	if timeout < base {
		return base
	}
	return timeout
}

func parseAnalyzedJob(content string, in events.RawJobItem) (events.AnalyzedJob, error) {
	var out events.AnalyzedJob
	if err := json.Unmarshal([]byte(content), &out); err != nil {
		return events.AnalyzedJob{}, fmt.Errorf("json parse failed: %w; content=%s", err, content)
	}
	if out.Role == "" || out.Company == "" || out.SummaryIT == "" {
		return events.AnalyzedJob{}, errors.New("missing required analyzed fields")
	}
	out.SchemaVersion = events.SchemaVersion
	out.ID = in.ID
	out.GUID = in.GUID
	out.SourceURL = in.URL
	out.SourceLabel = in.SourceLabel
	out.Title = in.Title
	out.OriginalLinks = uniqueURLs([]string{in.URL}, in.Links)
	out.OriginalImages = uniqueURLs(in.ImageURLs)
	out.JobCategory = normalizeJobCategory(out.JobCategory)
	out.Seniority = sanitizeUnknown(out.Seniority)
	out.Location = sanitizeUnknown(out.Location)
	out.ContractType = sanitizeUnknown(out.ContractType)
	out.Salary = sanitizeUnknown(out.Salary)
	out.TechStack = trimList(out.TechStack, 8)
	out.TechStack = sanitizeTechStack(out.TechStack)
	out.Tags = trimList(out.Tags, 6)
	out.Tags = sanitizeTags(out.Tags)
	out.JobPostQualityScore, out.JobPostQualityRank, out.JobPostMissing = computeJobPostQuality(out)
	if out.Language == "" {
		out.Language = "it"
	}
	out.CreatedAt = time.Now().UTC()
	return out, nil
}

func isTruncatedJSONError(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unexpected end of json input")
}

func trimList(values []string, max int) []string {
	if max <= 0 || len(values) <= max {
		return values
	}
	return values[:max]
}

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

func uniqueURLs(values ...[]string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	for _, list := range values {
		for _, v := range list {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if _, ok := seen[v]; ok {
				continue
			}
			seen[v] = struct{}{}
			out = append(out, v)
		}
	}
	return out
}

func normalizeJobCategory(v string) string {
	k := strings.ToLower(strings.TrimSpace(v))
	if k == "" {
		return "Altri ruoli"
	}
	if normalized, ok := normalizedJobCategories[k]; ok {
		return normalized
	}
	for _, allowed := range allowedJobCategories {
		if strings.EqualFold(strings.TrimSpace(allowed), k) {
			return allowed
		}
	}
	return "Altri ruoli"
}

func (s *Service) fetchSourcePageExcerpt(ctx context.Context, rawURL string) (string, error) {
	switch normalizeSourceExtractor(s.cfg.SourceExtractor) {
	case sourceExtractorOff:
		return "", nil
	case sourceExtractorBasic:
		return s.fetchSourcePageExcerptBasic(ctx, rawURL)
	case sourceExtractorScrapling:
		return s.fetchSourcePageExcerptScrapling(ctx, rawURL)
	case sourceExtractorHybrid:
		return s.fetchSourcePageExcerptHybrid(ctx, rawURL)
	default:
		return s.fetchSourcePageExcerptBasic(ctx, rawURL)
	}
}

func normalizeSourceExtractor(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", sourceExtractorBasic:
		return sourceExtractorBasic
	case sourceExtractorOff:
		return sourceExtractorOff
	case sourceExtractorScrapling:
		return sourceExtractorScrapling
	case sourceExtractorHybrid:
		return sourceExtractorHybrid
	default:
		return sourceExtractorBasic
	}
}

func (s *Service) fetchSourcePageExcerptHybrid(ctx context.Context, rawURL string) (string, error) {
	basicText, basicErr := s.fetchSourcePageExcerptBasic(ctx, rawURL)
	if basicErr == nil && utf8.RuneCountInString(strings.TrimSpace(basicText)) >= s.cfg.SourceMinChars {
		return basicText, nil
	}

	scraplingText, scraplingErr := s.fetchSourcePageExcerptScrapling(ctx, rawURL)
	if scraplingErr == nil {
		return scraplingText, nil
	}

	if basicErr == nil {
		// Keep best-effort basic text if fallback fails after short extraction.
		if strings.TrimSpace(basicText) != "" {
			return basicText, nil
		}
		return "", scraplingErr
	}
	return "", fmt.Errorf("basic failed: %w; scrapling failed: %v", basicErr, scraplingErr)
}

func (s *Service) fetchSourcePageExcerptBasic(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return "", fmt.Errorf("invalid source url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported source url scheme: %s", u.Scheme)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "rwct-agent-analyzer/1.0 (+source-page-scraper)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("source page status: %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, 256*1024)
	b, err := io.ReadAll(limited)
	if err != nil {
		return "", err
	}
	txt := extractTextFromHTML(string(b))
	txt = clampString(txt, s.sourceExcerptMaxChars())
	return txt, nil
}

type scraplingExtractRequest struct {
	URL       string `json:"url"`
	TimeoutMS int    `json:"timeout_ms"`
	MaxChars  int    `json:"max_chars"`
	Mode      string `json:"mode"`
	UserAgent string `json:"user_agent"`
}

type scraplingExtractResponse struct {
	OK         bool   `json:"ok"`
	Text       string `json:"text"`
	FinalURL   string `json:"final_url"`
	Method     string `json:"method"`
	StatusCode int    `json:"status_code"`
	DurationMS int    `json:"duration_ms"`
	Blocked    bool   `json:"blocked"`
	Error      string `json:"error"`
}

func (s *Service) fetchSourcePageExcerptScrapling(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", nil
	}
	u, err := url.Parse(rawURL)
	if err != nil || u == nil {
		return "", fmt.Errorf("invalid source url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported source url scheme: %s", u.Scheme)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(s.cfg.ScraplingEndpoint), "/")
	if endpoint == "" {
		return "", errors.New("missing scrapling endpoint")
	}

	timeout := s.cfg.ScraplingTimeout
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	reqBody := scraplingExtractRequest{
		URL:       rawURL,
		TimeoutMS: int(timeout.Milliseconds()),
		MaxChars:  s.cfg.ScraplingMaxChars,
		Mode:      "auto",
		UserAgent: "rwct-agent-analyzer/1.0 (+source-page-scraper)",
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint+"/extract", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("scrapling status: %d", resp.StatusCode)
	}

	var out scraplingExtractResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.OK {
		msg := strings.TrimSpace(out.Error)
		if msg == "" {
			msg = "extract failed"
		}
		return "", fmt.Errorf("scrapling extract failed: %s", msg)
	}

	txt := clampString(strings.TrimSpace(out.Text), s.cfg.ScraplingMaxChars)
	if txt == "" {
		return "", errors.New("scrapling returned empty text")
	}
	return txt, nil
}

func extractTextFromHTML(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	clean := reScriptTags.ReplaceAllString(raw, " ")
	clean = reStyleTags.ReplaceAllString(clean, " ")
	clean = reHTMLTags.ReplaceAllString(clean, " ")
	clean = html.UnescapeString(clean)
	clean = strings.Join(strings.Fields(clean), " ")
	return strings.TrimSpace(clean)
}

func clampString(v string, max int) string {
	v = strings.TrimSpace(v)
	if max <= 0 || len(v) <= max {
		return v
	}
	return strings.TrimSpace(v[:max])
}

func (s *Service) shouldQuarantineClaim(itemID string, err error) bool {
	if isNonRetryableAnalysisErr(err) {
		return true
	}
	deliveryCount, loadErr := s.currentClaimDeliveryCount(itemID)
	if loadErr != nil {
		slog.Warn("cannot read claim delivery_count; fallback to requeue", "item_id", itemID, "err", loadErr)
		return false
	}
	return deliveryCount >= s.cfg.MaxDeliveryAttempts
}

func (s *Service) currentClaimDeliveryCount(itemID string) (int, error) {
	var count int
	err := s.db.QueryRow(
		`SELECT COALESCE(delivery_count, 0)
FROM analyzer_queue
WHERE item_id = ? AND lease_owner = ?`,
		strings.TrimSpace(itemID), s.workerID,
	).Scan(&count)
	if err != nil {
		return 0, err
	}
	return count, nil
}

func isNonRetryableAnalysisErr(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(s, "missing required analyzed fields")
}

func (s *Service) upsertRuntimeSnapshot() {
	now := time.Now().UTC()
	thinking := 0
	if s.cfg.LLMThinking {
		thinking = 1
	}
	_, err := s.db.Exec(`INSERT INTO analyzer_runtime(
worker_id, name, model, endpoint, timeout, max_tokens, thinking_enabled, max_concurrency, max_jobs_per_min, queue_poll_interval, lease_duration, updated_at
) VALUES(?, 'job-analyzer', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(worker_id) DO UPDATE SET
 name=excluded.name,
 model=excluded.model,
 endpoint=excluded.endpoint,
 timeout=excluded.timeout,
 max_tokens=excluded.max_tokens,
 thinking_enabled=excluded.thinking_enabled,
 max_concurrency=excluded.max_concurrency,
 max_jobs_per_min=excluded.max_jobs_per_min,
 queue_poll_interval=excluded.queue_poll_interval,
 lease_duration=excluded.lease_duration,
 updated_at=excluded.updated_at`,
		s.workerID,
		strings.TrimSpace(s.cfg.LLMModel),
		strings.TrimSpace(s.cfg.LLMEndpoint),
		s.cfg.LLMTimeout.String(),
		s.cfg.LLMMaxTokens,
		thinking,
		s.cfg.MaxConcurrency,
		s.cfg.MaxJobsPerMin,
		s.cfg.QueuePollInterval.String(),
		s.cfg.LeaseDuration.String(),
		now,
	)
	if err != nil {
		slog.Warn("upsert analyzer runtime snapshot failed", "worker_id", s.workerID, "err", err)
	}
}

func sanitizeUnknown(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	lv := strings.ToLower(v)
	placeholders := []string{
		"unknown", "n/a", "na", "none", "null", "-", "--",
		"not specified", "not available", "unspecified", "tbd",
		"non specificato", "non disponibile", "non indicato", "da definire", "nessuno",
	}
	for _, p := range placeholders {
		if lv == p {
			return ""
		}
	}
	return v
}

func sanitizeTechStack(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, v := range values {
		clean := sanitizeUnknown(v)
		if clean == "" {
			continue
		}
		key := strings.ToLower(clean)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, clean)
	}
	return out
}

func sanitizeTags(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, v := range values {
		tag := normalizeTag(v)
		if tag == "" {
			continue
		}
		if len(tag) > 24 {
			tag = tag[:24]
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func normalizeTag(v string) string {
	v = strings.TrimSpace(strings.TrimPrefix(v, "#"))
	if v == "" {
		return ""
	}
	mapped := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			return unicode.ToLower(r)
		case r == ' ' || r == '-' || r == '_':
			return '-'
		default:
			return -1
		}
	}, v)
	if mapped == "" {
		return ""
	}
	var b strings.Builder
	prevDash := false
	for _, r := range mapped {
		if r == '-' {
			if prevDash {
				continue
			}
			prevDash = true
			b.WriteRune(r)
			continue
		}
		prevDash = false
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-")
}

func computeJobPostQuality(out events.AnalyzedJob) (score int, rank string, missing []string) {
	type fieldCheck struct {
		name string
		ok   bool
	}
	checks := []fieldCheck{
		{name: "seniority", ok: sanitizeUnknown(out.Seniority) != ""},
		{name: "location", ok: sanitizeUnknown(out.Location) != ""},
		{name: "contract_type", ok: sanitizeUnknown(out.ContractType) != ""},
		{name: "salary", ok: hasClearSalary(out.Salary)},
		{name: "tech_stack", ok: len(sanitizeTechStack(out.TechStack)) > 0},
	}
	for _, c := range checks {
		if c.ok {
			score += 20
			continue
		}
		missing = append(missing, c.name)
	}
	switch {
	case score >= 90:
		rank = "A"
	case score >= 70:
		rank = "B"
	case score >= 50:
		rank = "C"
	default:
		rank = "D"
	}
	return score, rank, missing
}

func hasClearSalary(v string) bool {
	v = sanitizeUnknown(v)
	if v == "" {
		return false
	}
	lv := strings.ToLower(v)
	ambiguous := []string{
		"competitive", "commisurata", "negoziabile", "market rate", "depends", "depending",
		"to be defined", "da definire", "tbd",
	}
	for _, token := range ambiguous {
		if strings.Contains(lv, token) {
			return false
		}
	}
	return strings.ContainsAny(v, "0123456789€$£")
}
