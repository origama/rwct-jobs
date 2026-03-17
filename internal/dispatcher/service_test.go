package dispatcher

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"rwct-agent/pkg/events"
)

func TestSanitizeTelegramHashtag(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want string
	}{
		{in: "Remote EU", want: "remoteeu"},
		{in: "full-time", want: "fulltime"},
		{in: "go_lang", want: "golang"},
		{in: "#C++", want: "c"},
		{in: "   ", want: ""},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			got := sanitizeTelegramHashtag(tc.in)
			if got != tc.want {
				t.Fatalf("sanitizeTelegramHashtag(%q)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTelegramTemplateRendersSanitizedHashtags(t *testing.T) {
	t.Parallel()

	svc, err := New(Config{
		DBPath:            filepath.Join(t.TempDir(), "dispatcher.db"),
		QueuePollInterval: 100 * time.Millisecond,
		LeaseDuration:     1 * time.Minute,
		DestinationMode:   "telegram",
		RateLimitPerMin:   60,
		RetryAttempts:     1,
		RetryBaseDelay:    50 * time.Millisecond,
		TemplatePath:      filepath.Join("..", "..", "configs", "message.tmpl.md"),
		TelegramTemplatePath: filepath.Join(
			"..", "..", "configs", "message.telegram.tmpl.md",
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.db.Close()

	in := events.AnalyzedJob{
		Role:         "Backend Engineer",
		Company:      "ACME",
		Location:     "Remote",
		Salary:       "n/a",
		TechStack:    []string{"Node JS", "C#", "ci_cd"},
		Tags:         []string{"remote-eu", "full time", "go_lang"},
		Seniority:    "mid",
		ContractType: "full-time",
		Language:     "it",
		SourceURL:    "https://example.com",
	}
	var msg bytes.Buffer
	if err := svc.tplTG.Execute(&msg, in); err != nil {
		t.Fatalf("telegram template execute failed: %v", err)
	}
	got := msg.String()
	for _, want := range []string{
		"#nodejs",
		"#c",
		"#cicd",
		"#remoteeu",
		"#fulltime",
		"#golang",
	} {
		if !bytes.Contains(msg.Bytes(), []byte(want)) {
			t.Fatalf("expected rendered telegram output to include %q, got: %s", want, got)
		}
	}
	for _, forbidden := range []string{
		"#remote-eu",
		"#full time",
		"#go_lang",
	} {
		if bytes.Contains(msg.Bytes(), []byte(forbidden)) {
			t.Fatalf("expected rendered telegram output to exclude %q, got: %s", forbidden, got)
		}
	}
}
