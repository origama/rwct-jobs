package analyzer

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/time/rate"
	_ "modernc.org/sqlite"

	"rwct-agent/pkg/events"
	"rwct-agent/pkg/retry"
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
	DBPath            string
	QueuePollInterval time.Duration
	LeaseDuration     time.Duration
	LLMEndpoint       string
	LLMModel          string
	LLMTimeout        time.Duration
	LLMMaxTokens      int
	LLMThinking       bool
	MaxConcurrency    int
	MaxJobsPerMin     int
	RetryAttempts     int
	RetryBaseDelay    time.Duration
}

type Service struct {
	cfg      Config
	db       *sql.DB
	http     *http.Client
	limiter  *rate.Limiter
	workerID string
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
	if cfg.QueuePollInterval <= 0 {
		cfg.QueuePollInterval = 1200 * time.Millisecond
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 2 * time.Minute
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
		cfg:      cfg,
		db:       db,
		http:     &http.Client{Timeout: cfg.LLMTimeout},
		limiter:  rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.MaxJobsPerMin)), 1),
		workerID: fmt.Sprintf("%s-%d", safeHostname(), time.Now().UTC().UnixNano()),
	}, nil
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
	var in events.RawJobItem
	if err := json.Unmarshal(payload, &in); err != nil {
		_ = s.releaseClaim(itemID, fmt.Sprintf("invalid_raw_payload: %v", err))
		return err
	}
	if strings.TrimSpace(in.ID) == "" {
		in.ID = itemID
	}

	if already, analyzedPayload, err := s.loadStoredAnalyzed(in.ID); err == nil && already {
		if err := s.enqueueDispatchJob(in.ID, []byte(analyzedPayload)); err != nil {
			_ = s.releaseClaim(itemID, fmt.Sprintf("enqueue_stored_dispatch_failed: %v", err))
			return err
		}
		return s.completeClaim(itemID)
	}

	var analyzed events.AnalyzedJob
	err := retry.Exponential(ctx, s.cfg.RetryAttempts, s.cfg.RetryBaseDelay, func(attempt int) error {
		out, e := s.analyze(ctx, in)
		if e != nil {
			_ = s.markItemStatus(in.ID, events.ItemStatusFailed, e.Error(), nil)
			s.publishError(in.ID, attempt, "llm_call_failed", e)
			return e
		}
		analyzed = out
		return nil
	})
	if err != nil {
		slog.Error("analysis failed after retries", "item_id", in.ID, "err", err)
		_ = s.releaseClaim(itemID, fmt.Sprintf("analysis_failed: %v", err))
		return err
	}

	_ = s.markItemStatus(in.ID, events.ItemStatusAnalyzed, "", &analyzed)
	body, _ := json.Marshal(analyzed)
	if err := s.enqueueDispatchJob(in.ID, body); err != nil {
		_ = s.releaseClaim(itemID, fmt.Sprintf("enqueue_dispatch_failed: %v", err))
		return err
	}
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
	content, err := s.callLLM(ctx, buildPrompt(in), s.cfg.LLMMaxTokens)
	if err != nil {
		return events.AnalyzedJob{}, err
	}
	out, parseErr := parseAnalyzedJob(content, in)
	if parseErr == nil {
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
		content2, err2 := s.callLLM(ctx, buildCompactPrompt(in), fallbackTokens)
		if err2 != nil {
			return events.AnalyzedJob{}, err2
		}
		out2, parseErr2 := parseAnalyzedJob(content2, in)
		if parseErr2 == nil {
			return out2, nil
		}
		return events.AnalyzedJob{}, parseErr2
	}
	return events.AnalyzedJob{}, parseErr
}

func buildPrompt(in events.RawJobItem) string {
	description := strings.TrimSpace(in.Description)
	if len(description) > 1800 {
		description = description[:1800]
	}
	return fmt.Sprintf(`Analizza il seguente annuncio lavoro e restituisci SOLO JSON valido con i campi:
job_category, role, company, seniority, location, remote_type, tech_stack (array), contract_type, salary, language, summary_it, confidence.
Lingua output: italiano.
Se un campo non è presente usa stringa vuota (o array vuoto).
Mantieni tech_stack breve: massimo 8 tecnologie reali, senza inventare valori.
summary_it: una sola frase, massimo 220 caratteri.
Per job_category usa SOLO una di queste categorie esatte:
%s
Se non è classificabile, usa esattamente: Altri ruoli.

Titolo: %s
URL: %s
Descrizione: %s
Link estratti: %s
Immagini estratte: %s`, strings.Join(allowedJobCategories, ", "), in.Title, in.URL, description, strings.Join(in.Links, ", "), strings.Join(in.ImageURLs, ", "))
}

func buildCompactPrompt(in events.RawJobItem) string {
	description := strings.TrimSpace(in.Description)
	if len(description) > 1200 {
		description = description[:1200]
	}
	return fmt.Sprintf(`Restituisci SOLO 1 riga JSON valida (nessun markdown, nessun testo extra) con questi campi:
job_category,role,company,seniority,location,remote_type,tech_stack,contract_type,salary,language,summary_it,confidence
Vincoli:
- job_category deve essere una di: %s
- tech_stack massimo 8 elementi
- summary_it massimo 220 caratteri

Titolo: %s
URL: %s
Descrizione: %s`, strings.Join(allowedJobCategories, ", "), in.Title, in.URL, description)
}

func stripMarkdownCodeFence(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "```json")
	v = strings.TrimPrefix(v, "```")
	v = strings.TrimSuffix(v, "```")
	return strings.TrimSpace(v)
}

func (s *Service) callLLM(ctx context.Context, prompt string, maxTokens int) (string, error) {
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
	body, _ := json.Marshal(reqBody)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.cfg.LLMEndpoint, "/")+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("llm status: %d", resp.StatusCode)
	}

	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", err
	}
	if len(r.Choices) == 0 {
		return "", errors.New("empty llm response")
	}
	return stripMarkdownCodeFence(strings.TrimSpace(r.Choices[0].Message.Content)), nil
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
	out.TechStack = trimList(out.TechStack, 8)
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
