package main

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort            = 8095
	feedPath               = "/feed.rss"
	healthPath             = "/healthz"
	jobsPathPrefix         = "/jobs/"
	defaultPublishInterval = 5 * time.Minute
	defaultMaxFeedItems    = 10
	feedTitle              = "RWCT Test Jobs Feed"
	feedDescription        = "Synthetic remote jobs feed and local job pages for end-to-end scraping tests"
	feedLanguage           = "en-us"
	feedCopyright          = "(c) RWCT Test Feed"
	generatorName          = "rwct-test-rss"
	defaultTestSiteTitle   = "RWCT Test Jobs"
	defaultContractType    = "Full-Time"
	defaultCategory        = "Programming"
)

type config struct {
	Port            int
	BaseURL         string
	PublishInterval time.Duration
	MaxFeedItems    int
	SiteTitle       string
}

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Atom    string     `xml:"xmlns:atom,attr,omitempty"`
	Channel rssChannel `xml:"channel"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssChannel struct {
	Title         string    `xml:"title"`
	Link          string    `xml:"link"`
	Description   string    `xml:"description"`
	Language      string    `xml:"language"`
	Copyright     string    `xml:"copyright"`
	LastBuildDate string    `xml:"lastBuildDate"`
	Generator     string    `xml:"generator"`
	AtomLink      atomLink  `xml:"atom:link"`
	Items         []rssItem `xml:"item"`
}

type guid struct {
	IsPermaLink string `xml:"isPermaLink,attr,omitempty"`
	Value       string `xml:",chardata"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        guid   `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Category    string `xml:"category,omitempty"`
	Description string `xml:"description"`
}

type slot struct {
	Index int
	Time  time.Time
}

type jobPost struct {
	Index        int
	PublishedAt  time.Time
	Company      string
	Role         string
	Region       string
	Location     string
	SalaryMinK   int
	SalaryMaxK   int
	Slug         string
	URL          string
	Title        string
	Description  string
	Category     string
	ContractType string
	Stack        []string
}

var (
	companies = []string{
		"Acme Cloud",
		"Nimbus Labs",
		"Northstar Data",
		"Pixel Orbit",
		"Atlas Metrics",
	}

	roles = []string{
		"Senior Go Backend Engineer",
		"Staff Platform Engineer",
		"DevOps Engineer (Kubernetes)",
		"Full-Stack Engineer (Go/React)",
		"Site Reliability Engineer",
	}

	regions = []string{
		"Worldwide",
		"EMEA",
		"US/Canada",
		"Europe",
		"LATAM",
	}

	stackPool = []string{
		"Go",
		"PostgreSQL",
		"Docker",
		"Kubernetes",
		"RabbitMQ",
		"Redis",
		"Terraform",
		"AWS",
		"GCP",
	}

	slugIndexRe = regexp.MustCompile(`-(\d+)$`)
)

func main() {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))

	cfg := config{
		Port:            getenvInt("TEST_RSS_PORT", defaultPort),
		PublishInterval: getenvDuration("TEST_RSS_PUBLISH_INTERVAL", defaultPublishInterval),
		MaxFeedItems:    getenvInt("TEST_RSS_MAX_FEED_ITEMS", defaultMaxFeedItems),
		SiteTitle:       getenv("TEST_RSS_SITE_TITLE", defaultTestSiteTitle),
	}
	if cfg.MaxFeedItems < 1 {
		cfg.MaxFeedItems = defaultMaxFeedItems
	}
	if cfg.PublishInterval < time.Second {
		cfg.PublishInterval = defaultPublishInterval
	}

	cfg.BaseURL = strings.TrimRight(getenv("TEST_RSS_BASE_URL", fmt.Sprintf("http://localhost:%d", cfg.Port)), "/")

	listenAddr := ":" + strconv.Itoa(cfg.Port)
	mux := http.NewServeMux()

	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc(feedPath, func(w http.ResponseWriter, r *http.Request) {
		serveFeed(w, r, cfg)
	})
	mux.HandleFunc(jobsPathPrefix, func(w http.ResponseWriter, r *http.Request) {
		serveJobPage(w, r, cfg)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveIndex(w, r, cfg)
	})

	slog.Info(
		"test rss site started",
		"addr", listenAddr,
		"base_url", cfg.BaseURL,
		"feed_path", feedPath,
		"jobs_path_prefix", jobsPathPrefix,
		"publish_interval", cfg.PublishInterval.String(),
		"max_feed_items", cfg.MaxFeedItems,
	)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		slog.Error("test rss site stopped", "err", err)
		os.Exit(1)
	}
}

func serveFeed(w http.ResponseWriter, _ *http.Request, cfg config) {
	now := time.Now().UTC()
	slots := generateSlots(now, cfg.PublishInterval, cfg.MaxFeedItems)
	items := make([]rssItem, 0, len(slots))
	for _, s := range slots {
		job := buildJob(s, cfg.BaseURL)
		items = append(items, rssItem{
			Title:    job.Title,
			Link:     job.URL,
			GUID:     guid{IsPermaLink: "true", Value: job.URL},
			PubDate:  job.PublishedAt.Format(time.RFC1123Z),
			Category: job.Category,
			Description: fmt.Sprintf(
				"<p><strong>%s</strong> is hiring a <strong>%s</strong>.</p><p><strong>Location:</strong> %s</p><p><strong>Type:</strong> %s</p><p><strong>Compensation:</strong> EUR %dk-%dk</p><p><strong>Stack:</strong> %s</p><p><a href=\"%s\">View local posting</a></p>",
				job.Company,
				job.Role,
				job.Location,
				job.ContractType,
				job.SalaryMinK,
				job.SalaryMaxK,
				strings.Join(job.Stack, ", "),
				job.URL,
			),
		})
	}

	payload := rss{
		Version: "2.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: rssChannel{
			Title:         feedTitle,
			Link:          cfg.BaseURL,
			Description:   feedDescription,
			Language:      feedLanguage,
			Copyright:     feedCopyright,
			LastBuildDate: now.Format(time.RFC1123Z),
			Generator:     generatorName,
			AtomLink: atomLink{
				Href: cfg.BaseURL + feedPath,
				Rel:  "self",
				Type: "application/rss+xml",
			},
			Items: items,
		},
	}

	body, err := xml.MarshalIndent(payload, "", "  ")
	if err != nil {
		http.Error(w, "failed to build RSS", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

func serveIndex(w http.ResponseWriter, r *http.Request, cfg config) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	jobs := generateLatestJobs(time.Now().UTC(), cfg)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>" + cfg.SiteTitle + "</title>"))
	_, _ = w.Write([]byte("<style>body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;max-width:860px;margin:24px auto;padding:0 12px;line-height:1.45;color:#0f172a}h1{margin-bottom:8px}a{color:#0ea5e9;text-decoration:none}a:hover{text-decoration:underline}.muted{color:#475569}.job{border:1px solid #e2e8f0;border-radius:10px;padding:12px 14px;margin:10px 0}</style></head><body>"))
	_, _ = w.Write([]byte("<h1>" + cfg.SiteTitle + "</h1>"))
	_, _ = w.Write([]byte("<p class=\"muted\">Feed RSS: <a href=\"" + feedPath + "\">" + feedPath + "</a> · Nuovo annuncio ogni " + cfg.PublishInterval.String() + "</p>"))
	for _, job := range jobs {
		_, _ = w.Write([]byte("<article class=\"job\">"))
		_, _ = w.Write([]byte("<h2><a href=\"" + jobPath(job.Slug) + "\">" + job.Title + "</a></h2>"))
		_, _ = w.Write([]byte("<p class=\"muted\">" + job.Company + " · " + job.Location + " · Pub: " + job.PublishedAt.Format(time.RFC3339) + "</p>"))
		_, _ = w.Write([]byte("</article>"))
	}
	_, _ = w.Write([]byte("</body></html>"))
}

func serveJobPage(w http.ResponseWriter, r *http.Request, cfg config) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if !strings.HasPrefix(r.URL.Path, jobsPathPrefix) {
		http.NotFound(w, r)
		return
	}
	slug := strings.TrimSpace(strings.TrimPrefix(r.URL.Path, jobsPathPrefix))
	slug = strings.Trim(slug, "/")
	if slug == "" {
		http.NotFound(w, r)
		return
	}

	idx, ok := parseSlotIndexFromSlug(slug)
	if !ok {
		http.NotFound(w, r)
		return
	}

	t := time.Unix(0, int64(idx)*cfg.PublishInterval.Nanoseconds()).UTC()
	job := buildJob(slot{Index: idx, Time: t}, cfg.BaseURL)
	if job.Slug != slug {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte("<!doctype html><html><head><meta charset=\"utf-8\"><meta name=\"viewport\" content=\"width=device-width, initial-scale=1\"><title>" + job.Title + "</title>"))
	_, _ = w.Write([]byte("<style>body{font-family:system-ui,-apple-system,Segoe UI,Roboto,sans-serif;max-width:860px;margin:24px auto;padding:0 12px;line-height:1.5;color:#111827}h1{margin-bottom:4px}.meta{color:#475569}.pill{display:inline-block;border:1px solid #cbd5e1;border-radius:999px;padding:2px 8px;font-size:12px;margin-right:6px}.box{background:#f8fafc;border:1px solid #e2e8f0;border-radius:10px;padding:12px 14px;margin:12px 0}</style></head><body>"))
	_, _ = w.Write([]byte("<p><a href=\"/\">← All jobs</a> · <a href=\"" + feedPath + "\">RSS feed</a></p>"))
	_, _ = w.Write([]byte("<h1>" + job.Title + "</h1>"))
	_, _ = w.Write([]byte("<p class=\"meta\">" + job.Company + " · " + job.Location + " · Published: " + job.PublishedAt.Format(time.RFC3339) + "</p>"))
	_, _ = w.Write([]byte("<p><span class=\"pill\">" + job.ContractType + "</span><span class=\"pill\">EUR " + strconv.Itoa(job.SalaryMinK) + "k-" + strconv.Itoa(job.SalaryMaxK) + "k</span><span class=\"pill\">" + job.Category + "</span></p>"))
	_, _ = w.Write([]byte("<div class=\"box\"><strong>Tech stack:</strong> " + strings.Join(job.Stack, ", ") + "</div>"))
	_, _ = w.Write([]byte("<p>" + job.Description + "</p>"))
	_, _ = w.Write([]byte("<p><strong>How to apply:</strong> send your CV with subject <code>" + job.Slug + "</code>.</p>"))
	_, _ = w.Write([]byte("</body></html>"))
}

func generateLatestJobs(now time.Time, cfg config) []jobPost {
	slots := generateSlots(now, cfg.PublishInterval, cfg.MaxFeedItems)
	out := make([]jobPost, 0, len(slots))
	for _, s := range slots {
		out = append(out, buildJob(s, cfg.BaseURL))
	}
	return out
}

func generateSlots(now time.Time, interval time.Duration, maxItems int) []slot {
	current := now.UTC().Truncate(interval)
	out := make([]slot, 0, maxItems)
	for i := 0; i < maxItems; i++ {
		t := current.Add(-time.Duration(i) * interval)
		out = append(out, slot{Index: slotIndex(t, interval), Time: t})
	}
	return out
}

func slotIndex(t time.Time, interval time.Duration) int {
	if interval <= 0 {
		interval = defaultPublishInterval
	}
	return int(t.UTC().UnixNano() / interval.Nanoseconds())
}

func parseSlotIndexFromSlug(slug string) (int, bool) {
	m := slugIndexRe.FindStringSubmatch(strings.TrimSpace(slug))
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func buildJob(s slot, baseURL string) jobPost {
	company := companies[s.Index%len(companies)]
	role := roles[s.Index%len(roles)]
	region := regions[s.Index%len(regions)]
	location := fmt.Sprintf("Remote (%s)", region)
	salaryMin := 95 + (s.Index%5)*10
	salaryMax := 120 + (s.Index%5)*10
	stack := rotateStack(s.Index)
	slug := slugify(fmt.Sprintf("%s %s %d", company, role, s.Index))
	url := strings.TrimRight(baseURL, "/") + jobPath(slug)
	title := fmt.Sprintf("%s at %s", role, company)
	description := fmt.Sprintf(
		"%s is looking for a %s to scale distributed systems used by remote teams across %s. You will own backend services, CI/CD reliability, and production observability.",
		company,
		role,
		region,
	)

	return jobPost{
		Index:        s.Index,
		PublishedAt:  s.Time.UTC(),
		Company:      company,
		Role:         role,
		Region:       region,
		Location:     location,
		SalaryMinK:   salaryMin,
		SalaryMaxK:   salaryMax,
		Slug:         slug,
		URL:          url,
		Title:        title,
		Description:  description,
		Category:     defaultCategory,
		ContractType: defaultContractType,
		Stack:        stack,
	}
}

func rotateStack(idx int) []string {
	if len(stackPool) == 0 {
		return []string{"Go", "PostgreSQL"}
	}
	out := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		out = append(out, stackPool[(idx+i)%len(stackPool)])
	}
	return out
}

func jobPath(slug string) string {
	return jobsPathPrefix + slug
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	repl := strings.NewReplacer(
		" ", "-",
		"(", "",
		")", "",
		"/", "-",
		"_", "-",
		".", "",
		",", "",
		":", "",
	)
	s = repl.Replace(s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	return strings.Trim(s, "-")
}

func getenv(k, def string) string {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	return v
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

func getenvDuration(k string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(k))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}
