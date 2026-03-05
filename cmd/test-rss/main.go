package main

import (
	"encoding/xml"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultPort      = 8095
	feedPath         = "/feed.rss"
	healthPath       = "/healthz"
	publishInterval  = 5 * time.Minute
	maxFeedItems     = 10
	feedTitle        = "RWCT Test Jobs Feed"
	feedDescription  = "Synthetic remote jobs feed for end-to-end dedup tests"
	feedLanguage     = "en-us"
	feedCopyright    = "(c) RWCT Test Feed"
	generatorName    = "rwct-test-rss"
	baseExternalSite = "https://weworkremotely.com"
)

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

var companies = []string{
	"Acme Cloud",
	"Nimbus Labs",
	"Northstar Data",
	"Pixel Orbit",
	"Atlas Metrics",
}

var roles = []string{
	"Senior Go Backend Engineer",
	"Staff Platform Engineer",
	"DevOps Engineer (Kubernetes)",
	"Full-Stack Engineer (Go/React)",
	"Site Reliability Engineer",
}

var regions = []string{
	"Worldwide",
	"EMEA",
	"US/Canada",
	"Europe",
	"LATAM",
}

func main() {
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	slog.SetDefault(slog.New(h))

	port := getenvInt("TEST_RSS_PORT", defaultPort)
	listenAddr := ":" + strconv.Itoa(port)
	selfBaseURL := strings.TrimRight(getenv("TEST_RSS_BASE_URL", fmt.Sprintf("http://localhost:%d", port)), "/")

	mux := http.NewServeMux()
	mux.HandleFunc(healthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc(feedPath, func(w http.ResponseWriter, r *http.Request) {
		serveFeed(w, r, selfBaseURL)
	})

	slog.Info("test rss server started", "addr", listenAddr, "feed_path", feedPath)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
		slog.Error("test rss server stopped", "err", err)
		os.Exit(1)
	}
}

func serveFeed(w http.ResponseWriter, _ *http.Request, selfBaseURL string) {
	now := time.Now().UTC()
	slots := generateSlots(now)
	items := make([]rssItem, 0, len(slots))
	for _, s := range slots {
		items = append(items, buildItem(s))
	}

	payload := rss{
		Version: "2.0",
		Atom:    "http://www.w3.org/2005/Atom",
		Channel: rssChannel{
			Title:         feedTitle,
			Link:          baseExternalSite,
			Description:   feedDescription,
			Language:      feedLanguage,
			Copyright:     feedCopyright,
			LastBuildDate: now.Format(time.RFC1123Z),
			Generator:     generatorName,
			AtomLink: atomLink{
				Href: selfBaseURL + feedPath,
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

type slot struct {
	Index int
	Time  time.Time
}

func generateSlots(now time.Time) []slot {
	current := now.Truncate(publishInterval)
	out := make([]slot, 0, maxFeedItems)
	for i := 0; i < maxFeedItems; i++ {
		out = append(out, slot{
			Index: slotIndex(current.Add(-time.Duration(i) * publishInterval)),
			Time:  current.Add(-time.Duration(i) * publishInterval),
		})
	}
	return out
}

func slotIndex(t time.Time) int {
	return int(t.Unix() / int64(publishInterval.Seconds()))
}

func buildItem(s slot) rssItem {
	company := companies[s.Index%len(companies)]
	role := roles[s.Index%len(roles)]
	region := regions[s.Index%len(regions)]
	location := fmt.Sprintf("Remote (%s)", region)
	slug := slugify(fmt.Sprintf("%s %s %d", company, role, s.Index))
	link := fmt.Sprintf("%s/remote-jobs/%s", baseExternalSite, slug)
	title := fmt.Sprintf("%s at %s", role, company)
	description := fmt.Sprintf(
		"<p><strong>%s</strong> is hiring a <strong>%s</strong>.</p><p><strong>Location:</strong> %s</p><p><strong>Type:</strong> Full-Time</p><p><strong>Compensation:</strong> EUR %dk-%dk</p><p><strong>Stack:</strong> Go, PostgreSQL, Docker, Kubernetes</p><p><a href=\"%s\">Apply now</a></p>",
		company,
		role,
		location,
		95+(s.Index%5)*10,
		120+(s.Index%5)*10,
		link,
	)

	return rssItem{
		Title:       title,
		Link:        link,
		GUID:        guid{IsPermaLink: "true", Value: link},
		PubDate:     s.Time.Format(time.RFC1123Z),
		Category:    "Programming",
		Description: description,
	}
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
