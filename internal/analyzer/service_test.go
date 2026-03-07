package analyzer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestNormalizeJobCategory(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "exact", in: "Programmazione", want: "Programmazione"},
		{name: "case insensitive", in: "marketing", want: "Marketing"},
		{name: "alias devops", in: "devops/sysadmin", want: "Devops & Sysadmin"},
		{name: "english fallback alias", in: "other roles", want: "Altri ruoli"},
		{name: "unknown defaults", in: "data science", want: "Altri ruoli"},
		{name: "empty defaults", in: "", want: "Altri ruoli"},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeJobCategory(tc.in)
			if got != tc.want {
				t.Fatalf("normalizeJobCategory(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCallLLMIncludesEnableThinkingWhenEnabled(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"role\":\"Software Engineer\"}"}}]}`))
	}))
	defer srv.Close()

	svc := &Service{
		cfg:  Config{LLMEndpoint: srv.URL, LLMModel: "test-model", LLMThinking: true},
		http: &http.Client{Timeout: time.Second},
	}

	if _, err := svc.callLLM(context.Background(), "prompt", 123); err != nil {
		t.Fatalf("callLLM returned error: %v", err)
	}

	if got["enable_thinking"] != true {
		t.Fatalf("expected enable_thinking=true in request, got %#v", got["enable_thinking"])
	}
}

func TestCallLLMDoesNotIncludeEnableThinkingWhenDisabled(t *testing.T) {
	t.Parallel()

	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"role\":\"Software Engineer\"}"}}]}`))
	}))
	defer srv.Close()

	svc := &Service{
		cfg:  Config{LLMEndpoint: srv.URL, LLMModel: "test-model", LLMThinking: false},
		http: &http.Client{Timeout: time.Second},
	}

	if _, err := svc.callLLM(context.Background(), "prompt", 123); err != nil {
		t.Fatalf("callLLM returned error: %v", err)
	}

	if _, exists := got["enable_thinking"]; exists {
		t.Fatalf("expected enable_thinking to be omitted, got %#v", got["enable_thinking"])
	}
}
