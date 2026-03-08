package rssreader

import (
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	_ "modernc.org/sqlite"

	"rwct-agent/pkg/events"
	"rwct-agent/pkg/retry"
)

var (
	hrefRe       = regexp.MustCompile(`(?i)href=["']([^"']+)["']`)
	srcRe        = regexp.MustCompile(`(?i)src=["']([^"']+)["']`)
	unsafeNameRe = regexp.MustCompile(`[^a-z0-9]+`)
)

type Config struct {
	DBPath                string
	FeedsFile             string
	FeedsCSV              string
	BootstrapMarkExisting bool
	ColdStartItemsPerFeed int
	PollInterval          time.Duration
	RetryAttempts         int
	RetryBaseDelay        time.Duration
	CooldownRetry         time.Duration
	HealthPort            int
	CleanupEvery          time.Duration
	RetentionHours        int
	MaxItemsPerPoll       int
}

type Service struct {
	cfg    Config
	db     *sql.DB
	parser *gofeed.Parser
}

func New(cfg Config) (*Service, error) {
	db, err := sql.Open("sqlite", sqliteDSN(cfg.DBPath))
	if err != nil {
		return nil, err
	}
	if err := initDB(db); err != nil {
		return nil, err
	}
	return &Service{cfg: cfg, db: db, parser: gofeed.NewParser()}, nil
}

func sqliteDSN(path string) string {
	if strings.Contains(path, "?") {
		return path + "&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	}
	return path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
}

func (s *Service) Close() error { return s.db.Close() }

func initDB(db *sql.DB) error {
	schema := `
CREATE TABLE IF NOT EXISTS processed_items (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 guid TEXT,
 url TEXT NOT NULL,
 title_norm TEXT NOT NULL,
 created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_items_guid ON processed_items(guid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_items_url ON processed_items(url);
CREATE INDEX IF NOT EXISTS idx_items_title_norm ON processed_items(title_norm);

CREATE TABLE IF NOT EXISTS pending_items (
 id TEXT PRIMARY KEY,
 guid TEXT,
 url TEXT NOT NULL,
 title_norm TEXT NOT NULL,
 created_at DATETIME NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_pending_guid ON pending_items(guid);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pending_url ON pending_items(url);
CREATE INDEX IF NOT EXISTS idx_pending_title_norm ON pending_items(title_norm);

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

CREATE TABLE IF NOT EXISTS rss_feeds (
 feed_url TEXT PRIMARY KEY,
 enabled INTEGER NOT NULL DEFAULT 1,
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL
);

CREATE TABLE IF NOT EXISTS rss_items (
 id TEXT PRIMARY KEY,
 source_feed_url TEXT NOT NULL,
 guid TEXT,
 url TEXT NOT NULL,
 title TEXT NOT NULL,
 title_norm TEXT NOT NULL,
 item_hash TEXT NOT NULL UNIQUE,
 description TEXT,
 links_json TEXT NOT NULL DEFAULT '[]',
 image_urls_json TEXT NOT NULL DEFAULT '[]',
 published_at DATETIME NOT NULL,
 fetched_at DATETIME NOT NULL,
 raw_enqueued_at DATETIME,
 raw_enqueue_count INTEGER NOT NULL DEFAULT 0,
 analyzed_enqueued_at DATETIME,
 analyzed_enqueue_count INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL,
 analyzed_at DATETIME,
 analyzed_payload_json TEXT,
 dispatched_at DATETIME,
 last_error TEXT,
 created_at DATETIME NOT NULL,
 updated_at DATETIME NOT NULL,
 FOREIGN KEY(source_feed_url) REFERENCES rss_feeds(feed_url)
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
CREATE INDEX IF NOT EXISTS idx_analyzer_queue_state_enqueued ON analyzer_queue(state, enqueued_at);
CREATE INDEX IF NOT EXISTS idx_analyzer_queue_lease_until ON analyzer_queue(lease_until);
	`
	_, err := db.Exec(schema)
	if err != nil {
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
		`ALTER TABLE rss_items ADD COLUMN analyzed_payload_json TEXT`,
		`ALTER TABLE rss_items ADD COLUMN dispatched_at DATETIME`,
		`ALTER TABLE rss_items ADD COLUMN last_error TEXT`,
		`ALTER TABLE rss_items ADD COLUMN created_at DATETIME`,
	}
	for _, stmt := range statements {
		if _, err := db.Exec(stmt); err != nil && !strings.Contains(strings.ToLower(err.Error()), "duplicate column name") {
			return err
		}
	}
	_, _ = db.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS idx_rss_items_item_hash ON rss_items(item_hash)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_rss_items_source_feed_url ON rss_items(source_feed_url)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_rss_items_status ON rss_items(status)`)
	_, _ = db.Exec(`CREATE INDEX IF NOT EXISTS idx_rss_items_published_at ON rss_items(published_at)`)
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
	return nil
}

func (s *Service) Run(ctx context.Context) error {
	feeds, err := s.loadFeeds()
	if err != nil {
		return err
	}
	if len(feeds) == 0 {
		return errors.New("no feeds configured")
	}
	slog.Info("rss-reader started", "feeds", len(feeds), "poll_interval", s.cfg.PollInterval.String())

	if s.cfg.BootstrapMarkExisting {
		hasProcessed, err := s.hasAnyProcessed()
		if err != nil {
			return err
		}
		if !hasProcessed {
			published, marked, err := s.bootstrapMarkExisting(ctx, feeds)
			if err != nil {
				slog.Error("bootstrap mark existing failed", "err", err)
			} else {
				slog.Info("bootstrap completed", "items_published", published, "items_marked_processed", marked)
			}
		}
	}

	ticker := time.NewTicker(s.cfg.PollInterval)
	cleanup := time.NewTicker(s.cfg.CleanupEvery)
	forcePoll := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer cleanup.Stop()
	defer forcePoll.Stop()

	if err := s.pollAll(ctx, feeds); err != nil {
		slog.Error("initial poll failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			reloaded, err := s.loadFeeds()
			if err != nil {
				slog.Error("failed to reload feeds", "err", err)
				continue
			}
			if len(reloaded) == 0 {
				slog.Warn("feed list is empty")
				continue
			}
			feeds = reloaded
			_ = s.pollAll(ctx, feeds)
		case <-forcePoll.C:
			_ = s.processForcedPolls(ctx)
		case <-cleanup.C:
			_ = s.cleanup()
		}
	}
}

func (s *Service) hasAnyProcessed() (bool, error) {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM rss_items LIMIT 1`).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) bootstrapMarkExisting(ctx context.Context, feeds []string) (int, int, error) {
	totalPublished := 0
	totalMarked := 0
	for _, feedURL := range feeds {
		var feed *gofeed.Feed
		err := retry.Exponential(ctx, s.cfg.RetryAttempts, s.cfg.RetryBaseDelay, func(attempt int) error {
			parsed, e := s.parser.ParseURL(feedURL)
			if e != nil {
				slog.Warn("bootstrap feed parse attempt failed", "feed_url", feedURL, "attempt", attempt, "err", e)
				return e
			}
			feed = parsed
			return nil
		})
		if err != nil {
			slog.Error("bootstrap feed parse failed", "feed_url", feedURL, "err", err)
			continue
		}
		publishedForFeed := 0
		for _, item := range feed.Items {
			titleNorm := NormalizeTitle(item.Title)
			dup, derr := s.isDuplicate(item.GUID, item.Link, titleNorm)
			if derr != nil {
				continue
			}
			if dup {
				continue
			}
			shouldPublish := s.cfg.ColdStartItemsPerFeed > 0 && publishedForFeed < s.cfg.ColdStartItemsPerFeed
			if shouldPublish {
				evt := toRawEvent(feedURL, item)
				if err := s.enqueueAnalyzerJob(evt); err != nil {
					slog.Error("bootstrap enqueue raw failed", "feed_url", feedURL, "item_id", evt.ID, "err", err)
					continue
				}
				publishedForFeed++
				totalPublished++
			}
			if err := s.savePending(toRawEvent(feedURL, item).ID, item.GUID, item.Link, titleNorm); err != nil {
				continue
			}
			totalMarked++
		}
		slog.Info("bootstrap feed completed", "feed_url", feedURL, "items_published", publishedForFeed)
	}
	return totalPublished, totalMarked, nil
}

func (s *Service) pollAll(ctx context.Context, feeds []string) error {
	for _, feedURL := range feeds {
		if err := s.pollOne(ctx, feedURL); err != nil {
			slog.Error("feed polling failed", "feed_url", feedURL, "err", err)
		}
	}
	return nil
}

func (s *Service) pollOne(ctx context.Context, feedURL string) error {
	return s.pollOneWithMode(ctx, feedURL, false)
}

func (s *Service) pollOneWithMode(ctx context.Context, feedURL string, force bool) error {
	ready, err := s.canPoll(feedURL)
	if err != nil {
		return err
	}
	if !ready && !force {
		return nil
	}

	var feed *gofeed.Feed
	err = retry.Exponential(ctx, s.cfg.RetryAttempts, s.cfg.RetryBaseDelay, func(attempt int) error {
		parsed, e := s.parser.ParseURL(feedURL)
		if e != nil {
			slog.Warn("feed parse attempt failed", "feed_url", feedURL, "attempt", attempt, "err", e)
			return e
		}
		feed = parsed
		return nil
	})
	if err != nil {
		slog.Error("feed marked down after retries", "feed_url", feedURL, "err", err, "cooldown", s.cfg.CooldownRetry.String(), "force", force)
		return s.markDown(feedURL)
	}
	if err := s.markUp(feedURL); err != nil {
		slog.Warn("failed to mark feed up", "feed_url", feedURL, "err", err)
	}

	count := 0
	for _, item := range feed.Items {
		if s.cfg.MaxItemsPerPoll > 0 && count >= s.cfg.MaxItemsPerPoll {
			break
		}
		evt := toRawEvent(feedURL, item)
		inserted, ierr := s.insertItemIfNew(evt)
		if ierr != nil {
			slog.Error("failed to store item", "feed_url", feedURL, "err", ierr)
			continue
		}
		if !inserted {
			slog.Info("duplicate item skipped", "feed_url", feedURL, "guid", item.GUID, "url", item.Link, "title", item.Title, "item_hash", evt.ID)
			continue
		}
		if err := s.enqueueAnalyzerJob(evt); err != nil {
			slog.Error("enqueue raw failed", "feed_url", feedURL, "item_id", evt.ID, "err", err)
			_ = s.markItemStatus(evt.ID, events.ItemStatusFailed, fmt.Sprintf("enqueue_raw_failed: %v", err))
			continue
		}
		count++
	}
	slog.Info("feed polled", "feed_url", feedURL, "items_processed", count, "force", force)
	return nil
}

func (s *Service) processForcedPolls(ctx context.Context) error {
	rows, err := s.db.Query(`SELECT feed_url FROM feed_poll_requests ORDER BY requested_at ASC LIMIT 20`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var feedURL string
		if err := rows.Scan(&feedURL); err != nil {
			return err
		}
		feedURL = strings.TrimSpace(feedURL)
		if feedURL == "" {
			continue
		}
		if err := s.pollOneWithMode(ctx, feedURL, true); err != nil {
			slog.Error("forced feed poll failed", "feed_url", feedURL, "err", err)
		}
		_, _ = s.db.Exec(`DELETE FROM feed_poll_requests WHERE feed_url = ?`, feedURL)
	}
	return rows.Err()
}

func toRawEvent(feedURL string, item *gofeed.Item) events.RawJobItem {
	pub := time.Now().UTC()
	if item.PublishedParsed != nil {
		pub = item.PublishedParsed.UTC()
	}
	if item.UpdatedParsed != nil && pub.IsZero() {
		pub = item.UpdatedParsed.UTC()
	}
	links, images := extractLinksAndImages(item)
	return events.RawJobItem{
		SchemaVersion: events.SchemaVersion,
		ID:            computeItemHash(item.GUID, item.Link, item.Title),
		FeedURL:       feedURL,
		SourceLabel:   buildSourceLabel(feedURL),
		GUID:          strings.TrimSpace(item.GUID),
		URL:           strings.TrimSpace(item.Link),
		Title:         strings.TrimSpace(item.Title),
		Description:   strings.TrimSpace(item.Description),
		Links:         links,
		ImageURLs:     images,
		PublishedAt:   pub,
		FetchedAt:     time.Now().UTC(),
	}
}

func extractLinksAndImages(item *gofeed.Item) ([]string, []string) {
	linkSet := map[string]struct{}{}
	imageSet := map[string]struct{}{}

	addLink := func(v string) {
		v = html.UnescapeString(strings.TrimSpace(v))
		if v == "" {
			return
		}
		if strings.HasPrefix(v, "//") {
			v = "https:" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return
		}
		u.Fragment = ""
		linkSet[u.String()] = struct{}{}
	}
	addImage := func(v string) {
		v = html.UnescapeString(strings.TrimSpace(v))
		if v == "" {
			return
		}
		if strings.HasPrefix(v, "//") {
			v = "https:" + v
		}
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return
		}
		u.Fragment = ""
		imageSet[u.String()] = struct{}{}
	}

	addLink(item.Link)
	htmlBlob := item.Description + "\n" + item.Content
	for _, m := range hrefRe.FindAllStringSubmatch(htmlBlob, -1) {
		if len(m) > 1 {
			addLink(m[1])
		}
	}
	for _, m := range srcRe.FindAllStringSubmatch(htmlBlob, -1) {
		if len(m) > 1 {
			addImage(m[1])
		}
	}
	for _, enc := range item.Enclosures {
		addLink(enc.URL)
		if strings.HasPrefix(strings.ToLower(enc.Type), "image/") {
			addImage(enc.URL)
		}
	}

	links := make([]string, 0, len(linkSet))
	for k := range linkSet {
		links = append(links, k)
	}
	images := make([]string, 0, len(imageSet))
	for k := range imageSet {
		images = append(images, k)
	}
	sort.Strings(links)
	sort.Strings(images)
	return links, images
}

func buildSourceLabel(feedURL string) string {
	u, err := url.Parse(feedURL)
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

func NormalizeTitle(s string) string {
	v := strings.ToLower(strings.TrimSpace(s))
	v = strings.Join(strings.Fields(v), " ")
	return v
}

func computeItemHash(guid, rawURL, title string) string {
	input := strings.TrimSpace(guid) + "|" + normalizeURL(rawURL) + "|" + NormalizeTitle(title)
	h := sha1.Sum([]byte(input))
	return hex.EncodeToString(h[:])
}

func (s *Service) insertItemIfNew(evt events.RawJobItem) (bool, error) {
	linksJSON, _ := json.Marshal(evt.Links)
	imagesJSON, _ := json.Marshal(evt.ImageURLs)
	res, err := s.db.Exec(
		`INSERT INTO rss_items(
			id, source_feed_url, guid, url, title, title_norm, item_hash, description,
			links_json, image_urls_json, published_at, fetched_at, status, created_at, updated_at
		) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(item_hash) DO NOTHING`,
		evt.ID,
		strings.TrimSpace(evt.FeedURL),
		strings.TrimSpace(evt.GUID),
		normalizeURL(evt.URL),
		strings.TrimSpace(evt.Title),
		NormalizeTitle(evt.Title),
		evt.ID,
		strings.TrimSpace(evt.Description),
		string(linksJSON),
		string(imagesJSON),
		evt.PublishedAt.UTC(),
		evt.FetchedAt.UTC(),
		events.ItemStatusNew,
		time.Now().UTC(),
		time.Now().UTC(),
	)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected > 0, nil
}

func (s *Service) markItemStatus(itemID, status, lastError string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	now := time.Now().UTC()
	var analyzedAt any
	var dispatchedAt any
	switch status {
	case events.ItemStatusAnalyzed:
		analyzedAt = now
	case events.ItemStatusDispatched:
		dispatchedAt = now
	}
	_, err := s.db.Exec(
		`UPDATE rss_items
		SET status = ?, analyzed_at = COALESCE(?, analyzed_at), dispatched_at = COALESCE(?, dispatched_at), last_error = ?, updated_at = ?
		WHERE id = ?`,
		status, analyzedAt, dispatchedAt, strings.TrimSpace(lastError), now, itemID,
	)
	return err
}

func (s *Service) markRawEnqueued(itemID string) error {
	itemID = strings.TrimSpace(itemID)
	if itemID == "" {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.db.Exec(
		`UPDATE rss_items
		SET raw_enqueued_at = COALESCE(?, raw_enqueued_at),
		    raw_enqueue_count = COALESCE(raw_enqueue_count, 0) + 1,
		    status = ?,
		    last_error = '',
		    updated_at = ?
		WHERE id = ?`,
		now, events.ItemStatusNew, now, itemID,
	)
	return err
}

func (s *Service) enqueueAnalyzerJob(evt events.RawJobItem) error {
	if strings.TrimSpace(evt.ID) == "" {
		return fmt.Errorf("empty item id")
	}
	now := time.Now().UTC()
	payload, err := json.Marshal(evt)
	if err != nil {
		return err
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
		evt.ID,
		string(payload),
		now,
		now,
	)
	if err != nil {
		return err
	}
	return s.markRawEnqueued(evt.ID)
}

func (s *Service) isDuplicate(guid, rawURL, titleNorm string) (bool, error) {
	u := normalizeURL(rawURL)
	q := `SELECT 1 FROM processed_items WHERE (guid <> '' AND guid = ?) OR url = ? OR title_norm = ?
UNION
SELECT 1 FROM pending_items WHERE (guid <> '' AND guid = ?) OR url = ? OR title_norm = ?
LIMIT 1`
	var one int
	err := s.db.QueryRow(q, strings.TrimSpace(guid), u, titleNorm, strings.TrimSpace(guid), u, titleNorm).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Service) savePending(itemID, guid, rawURL, titleNorm string) error {
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("empty item id")
	}
	_, err := s.db.Exec(
		`INSERT INTO pending_items(id, guid, url, title_norm, created_at) VALUES(?,?,?,?,?)
ON CONFLICT(id) DO NOTHING`,
		strings.TrimSpace(itemID), strings.TrimSpace(guid), normalizeURL(rawURL), titleNorm, time.Now().UTC(),
	)
	return err
}

func (s *Service) handleProcessedReceipt(payload []byte) error {
	var in events.ProcessedReceipt
	if err := json.Unmarshal(payload, &in); err != nil {
		return err
	}
	if strings.TrimSpace(in.URL) == "" && strings.TrimSpace(in.ItemID) == "" {
		return fmt.Errorf("processed receipt missing identifiers")
	}
	itemID := strings.TrimSpace(in.ItemID)
	if itemID == "" {
		itemID = computeItemHash(in.GUID, in.URL, in.Title)
	}
	if err := s.markItemStatus(itemID, events.ItemStatusDispatched, ""); err != nil {
		return err
	}
	slog.Info("item marked processed", "item_id", itemID, "url", normalizeURL(in.URL), "destination", in.Destination)
	return nil
}

func normalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	return u.String()
}

func (s *Service) loadFeeds() ([]string, error) {
	configured := s.listConfiguredFeeds()
	if len(configured) > 0 {
		if err := s.seedFeedsIntoDB(configured); err != nil {
			return nil, err
		}
	}
	return s.loadFeedsFromDB()
}

func (s *Service) listConfiguredFeeds() []string {
	seen := map[string]struct{}{}
	out := make([]string, 0)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		if _, ok := seen[v]; ok {
			return
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	for _, item := range strings.Split(s.cfg.FeedsCSV, ",") {
		add(item)
	}
	if s.cfg.FeedsFile != "" {
		b, err := os.ReadFile(s.cfg.FeedsFile)
		if err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				line = strings.TrimSpace(line)
				if line == "" || strings.HasPrefix(line, "#") {
					continue
				}
				add(line)
			}
		}
	}
	return out
}

func (s *Service) seedFeedsIntoDB(feeds []string) error {
	for _, feedURL := range feeds {
		if err := s.addFeed(feedURL); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) addFeed(feedURL string) error {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil
	}
	_, err := s.db.Exec(
		`INSERT INTO rss_feeds(feed_url, enabled, created_at, updated_at) VALUES(?,1,?,?)
ON CONFLICT(feed_url) DO UPDATE SET updated_at=excluded.updated_at`,
		feedURL,
		time.Now().UTC(),
		time.Now().UTC(),
	)
	return err
}

func (s *Service) loadFeedsFromDB() ([]string, error) {
	rows, err := s.db.Query(`SELECT feed_url FROM rss_feeds WHERE enabled = 1 ORDER BY feed_url`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var feedURL string
		if err := rows.Scan(&feedURL); err != nil {
			return nil, err
		}
		out = append(out, strings.TrimSpace(feedURL))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Service) canPoll(feedURL string) (bool, error) {
	var downUntil sql.NullTime
	err := s.db.QueryRow(`SELECT down_until FROM feed_state WHERE feed_url = ?`, feedURL).Scan(&downUntil)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if downUntil.Valid && time.Now().UTC().Before(downUntil.Time) {
		return false, nil
	}
	return true, nil
}

func (s *Service) markDown(feedURL string) error {
	downUntil := time.Now().UTC().Add(s.cfg.CooldownRetry)
	_, err := s.db.Exec(`
INSERT INTO feed_state(feed_url, failure_count, down_until, updated_at)
VALUES(?, 5, ?, ?)
ON CONFLICT(feed_url) DO UPDATE SET
  failure_count=5,
  down_until=excluded.down_until,
  updated_at=excluded.updated_at
`, feedURL, downUntil, time.Now().UTC())
	return err
}

func (s *Service) markUp(feedURL string) error {
	_, err := s.db.Exec(`
INSERT INTO feed_state(feed_url, failure_count, down_until, updated_at)
VALUES(?, 0, NULL, ?)
ON CONFLICT(feed_url) DO UPDATE SET
  failure_count=0,
  down_until=NULL,
  updated_at=excluded.updated_at
`, feedURL, time.Now().UTC())
	return err
}

func (s *Service) cleanup() error {
	if s.cfg.RetentionHours <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(s.cfg.RetentionHours) * time.Hour)
	res, err := s.db.Exec(`DELETE FROM processed_items WHERE created_at < ?`, cutoff)
	if err != nil {
		return err
	}
	nProcessed, _ := res.RowsAffected()
	res2, err := s.db.Exec(`DELETE FROM pending_items WHERE created_at < ?`, cutoff)
	if err != nil {
		return err
	}
	nPending, _ := res2.RowsAffected()
	if nProcessed > 0 || nPending > 0 {
		slog.Info("cleanup completed", "deleted_processed_items", nProcessed, "deleted_pending_items", nPending)
	}
	if res3, err := s.db.Exec(`DELETE FROM rss_items WHERE created_at < ?`, cutoff); err == nil {
		if nItems, _ := res3.RowsAffected(); nItems > 0 {
			slog.Info("cleanup completed", "deleted_rss_items", nItems)
		}
	}
	return nil
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
