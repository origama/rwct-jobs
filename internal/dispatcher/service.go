package dispatcher

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/template"
	"time"

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

type Config struct {
	DBPath                 string
	QueuePollInterval      time.Duration
	LeaseDuration          time.Duration
	DestinationMode        string
	FileSinkPath           string
	TemplatePath           string
	TelegramTemplatePath   string
	RateLimitPerMin        int
	RetryAttempts          int
	RetryBaseDelay         time.Duration
	TelegramBotToken       string
	TelegramChatID         string
	TelegramThreadID       int
	TelegramParseMode      string
	TelegramDisablePreview bool
	TelegramAPIBaseURL     string
}

type Service struct {
	cfg      Config
	db       *sql.DB
	limiter  *rate.Limiter
	tpl      *template.Template
	tplTG    *template.Template
	http     *http.Client
	workerID string

	tracer trace.Tracer

	dispatchDurationSec otelmetric.Float64Histogram
	dispatchProcessed   otelmetric.Int64Counter
	dispatchFailed      otelmetric.Int64Counter
}

const defaultTpl = `*RWCT-JOBS*
{{md .SourceLabel}}

{{if .OriginalLinks}}{{index .OriginalLinks 0}}{{else}}{{.SourceURL}}{{end}}

*Categoria:* {{md .JobCategory}}
*{{md .Role}}* @ *{{md .Company}}*
{{md .SummaryIT}}

- Seniority: {{md .Seniority}}
- Location: {{md .Location}} ({{md .RemoteType}})
- Contratto: {{md .ContractType}}
- Salary: {{md .Salary}}
- Stack: {{range $i, $v := .TechStack}}{{if $i}}, {{end}}{{md $v}}{{end}}
{{if .Tags}}- Tags: {{range $i, $t := .Tags}}{{if $i}} {{end}}#{{md $t}}{{end}}
{{end}}

{{if .OriginalLinks}}
*Link Originali*
{{range .OriginalLinks}}- {{.}}
{{end}}{{end}}

{{if .OriginalImages}}
*Immagini*
{{range .OriginalImages}}- {{.}}
{{end}}{{end}}

[Annuncio originale]({{.SourceURL}})
`

func New(cfg Config) (*Service, error) {
	if strings.TrimSpace(cfg.DBPath) == "" {
		cfg.DBPath = ":memory:"
	}
	if cfg.RateLimitPerMin < 1 {
		cfg.RateLimitPerMin = 30
	}
	if cfg.QueuePollInterval <= 0 {
		cfg.QueuePollInterval = 1200 * time.Millisecond
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 2 * time.Minute
	}
	content := defaultTpl
	if cfg.TemplatePath != "" {
		if b, err := os.ReadFile(cfg.TemplatePath); err == nil {
			content = string(b)
		}
	}
	db, err := sql.Open("sqlite", sqliteDSN(cfg.DBPath))
	if err != nil {
		return nil, err
	}
	if err := initStateDB(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	tpl, err := template.New("dispatch").Funcs(template.FuncMap{
		"md": escapeMarkdownText,
	}).Parse(content)
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	tplTG := tpl
	if strings.TrimSpace(cfg.TelegramTemplatePath) != "" {
		b, err := os.ReadFile(cfg.TelegramTemplatePath)
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("read telegram template failed: %w", err)
		}
		tplTG, err = template.New("dispatch-telegram").Funcs(template.FuncMap{
			"md": escapeMarkdownText,
		}).Parse(string(b))
		if err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("parse telegram template failed: %w", err)
		}
	}
	return &Service{
		cfg:     cfg,
		db:      db,
		limiter: rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.RateLimitPerMin)), 1),
		tpl:     tpl,
		tplTG:   tplTG,
		http: &http.Client{
			Timeout:   20 * time.Second,
			Transport: telemetry.NewHTTPTransport(http.DefaultTransport),
		},
		workerID: fmt.Sprintf("dispatcher-%d", time.Now().UTC().UnixNano()),
		tracer:   otel.Tracer("rwct-agent/message-dispatcher"),
		dispatchDurationSec: mustDispatchFloat64Histogram("rwct_dispatch_duration_seconds",
			"Duration of dispatch jobs"),
		dispatchProcessed: mustDispatchInt64Counter("rwct_dispatch_jobs_total",
			"Number of dispatch jobs processed"),
		dispatchFailed: mustDispatchInt64Counter("rwct_dispatch_job_failures_total",
			"Number of dispatch jobs failed"),
	}, nil
}

func dispatchMeter() otelmetric.Meter {
	return otel.Meter("rwct-agent/message-dispatcher")
}

func mustDispatchFloat64Histogram(name, description string) otelmetric.Float64Histogram {
	h, _ := dispatchMeter().Float64Histogram(name, otelmetric.WithDescription(description))
	return h
}

func mustDispatchInt64Counter(name, description string) otelmetric.Int64Counter {
	c, _ := dispatchMeter().Int64Counter(name, otelmetric.WithDescription(description))
	return c
}

func (s *Service) tracerForUse() trace.Tracer {
	if s.tracer != nil {
		return s.tracer
	}
	return otel.Tracer("rwct-agent/message-dispatcher")
}

func (s *Service) recordDispatchDuration(ctx context.Context, seconds float64, opts ...otelmetric.RecordOption) {
	if s.dispatchDurationSec != nil {
		s.dispatchDurationSec.Record(ctx, seconds, opts...)
	}
}

func (s *Service) addDispatchProcessed(ctx context.Context, opts ...otelmetric.AddOption) {
	if s.dispatchProcessed != nil {
		s.dispatchProcessed.Add(ctx, 1, opts...)
	}
}

func (s *Service) addDispatchFailed(ctx context.Context, opts ...otelmetric.AddOption) {
	if s.dispatchFailed != nil {
		s.dispatchFailed.Add(ctx, 1, opts...)
	}
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
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
	slog.Info("message-dispatcher started", "queue", "dispatch_queue", "worker_id", s.workerID, "destination", s.cfg.DestinationMode)
	defer s.db.Close()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		itemID, payload, ok, err := s.claimNextDispatchJob(ctx)
		if err != nil {
			slog.Error("claim dispatch job failed", "worker_id", s.workerID, "err", err)
			time.Sleep(s.cfg.QueuePollInterval)
			continue
		}
		if !ok {
			time.Sleep(s.cfg.QueuePollInterval)
			continue
		}
		if err := s.limiter.Wait(ctx); err != nil {
			_ = s.releaseDispatchClaim(itemID, fmt.Sprintf("rate limiter wait failed: %v", err))
			return err
		}
		if err := s.handleClaimedMessage(ctx, itemID, payload); err != nil {
			slog.Error("dispatch failed", "item_id", itemID, "worker_id", s.workerID, "err", err)
		}
	}
}

func (s *Service) handleClaimedMessage(ctx context.Context, itemID string, payload []byte) error {
	started := time.Now()
	var in events.AnalyzedJob
	if err := json.Unmarshal(payload, &in); err != nil {
		_ = s.releaseDispatchClaim(itemID, fmt.Sprintf("invalid_dispatch_payload: %v", err))
		s.addDispatchFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "invalid_dispatch_payload")))
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		in.ID = itemID
	}
	ctx = telemetry.ExtractTraceContext(ctx, in.TraceParent, in.TraceState)
	ctx, span := s.tracerForUse().Start(ctx, "dispatcher.handle_item",
		trace.WithAttributes(
			attribute.String("item.id", in.ID),
			attribute.String("destination.mode", strings.ToLower(strings.TrimSpace(s.cfg.DestinationMode))),
		))
	defer span.End()
	defer s.recordDispatchDuration(ctx, time.Since(started).Seconds())

	alreadyDispatched, err := s.isAlreadyDispatched(in.ID)
	if err != nil {
		span.RecordError(err)
		s.addDispatchFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "already_dispatched_check_failed")))
		return err
	}
	if alreadyDispatched {
		slog.Info("skip duplicate analyzed item already dispatched", "item_id", in.ID)
		s.addDispatchProcessed(ctx, otelmetric.WithAttributes(attribute.String("status", "duplicate")))
		return s.completeDispatchClaim(itemID)
	}
	tpl := s.tpl
	if strings.EqualFold(strings.TrimSpace(s.cfg.DestinationMode), "telegram") {
		tpl = s.tplTG
	}
	var msg bytes.Buffer
	if err := tpl.Execute(&msg, in); err != nil {
		_ = s.releaseDispatchClaim(itemID, fmt.Sprintf("template_execute_failed: %v", err))
		span.RecordError(err)
		s.addDispatchFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "template_execute_failed")))
		return err
	}

	err = retry.Exponential(ctx, s.cfg.RetryAttempts, s.cfg.RetryBaseDelay, func(attempt int) error {
		err := s.deliver(ctx, msg.String())
		if err != nil {
			_ = s.markItemStatus(in.ID, events.ItemStatusFailed, err.Error())
			s.publishError(in.ID, attempt, "dispatch_failed", err)
			span.RecordError(err)
		}
		return err
	})
	if err != nil {
		_ = s.releaseDispatchClaim(itemID, fmt.Sprintf("deliver_failed: %v", err))
		s.addDispatchFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "deliver_failed")))
		return err
	}
	_ = s.markItemStatus(in.ID, events.ItemStatusDispatched, "")
	if err := s.completeDispatchClaim(itemID); err != nil {
		span.RecordError(err)
		s.addDispatchFailed(ctx, otelmetric.WithAttributes(attribute.String("reason", "complete_claim_failed")))
		return err
	}
	s.addDispatchProcessed(ctx, otelmetric.WithAttributes(attribute.String("status", "success")))
	slog.Info("dispatch completed", "item_id", in.ID, "destination", strings.ToLower(s.cfg.DestinationMode))
	return nil
}

func (s *Service) isAlreadyDispatched(itemID string) (bool, error) {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return false, nil
	}
	var status string
	var dispatchedAt sql.NullString
	err := s.db.QueryRow(
		`SELECT COALESCE(status,''), dispatched_at FROM rss_items WHERE id = ?`,
		itemID,
	).Scan(&status, &dispatchedAt)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if strings.EqualFold(strings.TrimSpace(status), events.ItemStatusDispatched) {
		return true, nil
	}
	return dispatchedAt.Valid && strings.TrimSpace(dispatchedAt.String) != "", nil
}

func (s *Service) claimNextDispatchJob(ctx context.Context) (string, []byte, bool, error) {
	now := time.Now().UTC()
	leaseUntil := now.Add(s.cfg.LeaseDuration)
	row := s.db.QueryRowContext(ctx, `UPDATE dispatch_queue
SET state='LEASED',
    lease_owner=?,
    lease_until=?,
    delivery_count=COALESCE(delivery_count,0)+1,
    last_error='',
    updated_at=?
WHERE item_id = (
  SELECT item_id FROM dispatch_queue
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

func (s *Service) completeDispatchClaim(itemID string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE dispatch_queue
SET state='DONE',
    lease_owner=NULL,
    lease_until=NULL,
    done_at=COALESCE(done_at, ?),
    last_error='',
    updated_at=?
WHERE item_id = ? AND lease_owner = ?`, now, now, strings.TrimSpace(itemID), s.workerID)
	return err
}

func (s *Service) releaseDispatchClaim(itemID, reason string) error {
	now := time.Now().UTC()
	_, err := s.db.Exec(`UPDATE dispatch_queue
SET state='QUEUED',
    lease_owner=NULL,
    lease_until=NULL,
    last_error=?,
    updated_at=?
WHERE item_id = ? AND lease_owner = ?`, strings.TrimSpace(reason), now, strings.TrimSpace(itemID), s.workerID)
	return err
}

func (s *Service) markItemStatus(itemID, status, lastError string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	now := time.Now().UTC()
	var dispatchedAt any
	switch status {
	case events.ItemStatusDispatched:
		dispatchedAt = now
	}
	_, err := s.db.Exec(
		`UPDATE rss_items
		SET status = ?, dispatched_at = COALESCE(?, dispatched_at), last_error = ?, updated_at = ?
		WHERE id = ?`,
		status, dispatchedAt, strings.TrimSpace(lastError), now, itemID,
	)
	return err
}

func (s *Service) deliver(ctx context.Context, message string) error {
	switch strings.ToLower(s.cfg.DestinationMode) {
	case "file", "":
		return s.deliverFile(message)
	case "telegram":
		return s.deliverTelegram(ctx, message)
	default:
		return fmt.Errorf("unsupported destination mode: %s", s.cfg.DestinationMode)
	}
}

func (s *Service) deliverFile(message string) error {
	if s.cfg.FileSinkPath == "" {
		return fmt.Errorf("file sink path is empty")
	}
	if err := os.MkdirAll(filepathDir(s.cfg.FileSinkPath), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(s.cfg.FileSinkPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(time.Now().UTC().Format(time.RFC3339) + "\n" + message + "\n\n---\n\n")
	return err
}

func (s *Service) deliverTelegram(ctx context.Context, message string) error {
	if s.cfg.TelegramBotToken == "" {
		return fmt.Errorf("telegram bot token missing")
	}
	if s.cfg.TelegramChatID == "" {
		return fmt.Errorf("telegram chat id missing")
	}

	err := s.deliverTelegramWithParseMode(ctx, message, s.cfg.TelegramParseMode)
	if err == nil {
		return nil
	}
	// Fallback when formatting breaks Telegram entity parsing.
	if strings.TrimSpace(s.cfg.TelegramParseMode) != "" && strings.Contains(strings.ToLower(err.Error()), "can't parse entities") {
		slog.Warn("telegram parse failed, retrying without parse_mode")
		return s.deliverTelegramWithParseMode(ctx, message, "")
	}
	return err
}

func (s *Service) deliverTelegramWithParseMode(ctx context.Context, message, parseMode string) error {
	baseURL := strings.TrimRight(s.cfg.TelegramAPIBaseURL, "/")
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	apiURL := fmt.Sprintf("%s/bot%s/sendMessage", baseURL, s.cfg.TelegramBotToken)

	reqBody := map[string]any{
		"chat_id":                  s.cfg.TelegramChatID,
		"text":                     message,
		"disable_web_page_preview": s.cfg.TelegramDisablePreview,
	}
	if strings.TrimSpace(parseMode) != "" {
		reqBody["parse_mode"] = parseMode
	}
	if s.cfg.TelegramThreadID > 0 {
		reqBody["message_thread_id"] = s.cfg.TelegramThreadID
	}

	body, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))

	if resp.StatusCode >= 300 {
		return fmt.Errorf("telegram send failed status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var telegramResp struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &telegramResp); err != nil {
		return fmt.Errorf("telegram response decode failed: %w", err)
	}
	if !telegramResp.OK {
		return fmt.Errorf("telegram api not ok: %s", telegramResp.Description)
	}
	return nil
}

func filepathDir(path string) string {
	idx := strings.LastIndex(path, "/")
	if idx <= 0 {
		return "."
	}
	return path[:idx]
}

func escapeMarkdownText(s string) string {
	r := strings.NewReplacer(
		"\\", "\\\\",
		"_", "\\_",
		"*", "\\*",
		"[", "\\[",
		"`", "\\`",
	)
	return r.Replace(s)
}

func (s *Service) publishError(itemID string, attempt int, code string, err error) {
	slog.Warn("dispatcher processing error", "item_id", itemID, "attempt", attempt, "code", code, "err", err)
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

func MustEnvInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func MustEnvBool(key string, def bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return def
	}
	switch v {
	case "1", "true", "yes", "y", "on":
		return true
	case "0", "false", "no", "n", "off":
		return false
	default:
		return def
	}
}
