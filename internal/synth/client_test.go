package synth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseThinkingLevel(t *testing.T) {
	cases := []struct {
		in   string
		want ThinkingLevel
	}{
		{"", ""},
		{"  ", ""},
		{"minimal", ThinkingLevelMinimal},
		{"LOW", ThinkingLevelLow},
		{"Medium", ThinkingLevelMedium},
		{"high", ThinkingLevelHigh},
		{"XHigh", ThinkingLevelXHigh},
	}
	for _, tc := range cases {
		got, err := ParseThinkingLevel(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseThinkingLevel(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}
	if _, err := ParseThinkingLevel("ultra"); err == nil {
		t.Fatal("expected error for invalid thinking level")
	}
}

func TestVertexClientEndpointResolution(t *testing.T) {
	hc := &http.Client{}
	cGlobal, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:    "my-proj",
		Location:   "global",
		HTTPClient: hc,
	})
	if err != nil {
		t.Fatalf("NewVertexClient global: %v", err)
	}
	if cGlobal.baseURL != "https://aiplatform.googleapis.com" {
		t.Fatalf("global baseURL = %q, want https://aiplatform.googleapis.com", cGlobal.baseURL)
	}

	cReg, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:    "my-proj",
		Location:   "europe-west1",
		HTTPClient: hc,
	})
	if err != nil {
		t.Fatalf("NewVertexClient regional: %v", err)
	}
	if cReg.baseURL != "https://europe-west1-aiplatform.googleapis.com" {
		t.Fatalf("regional baseURL = %q, want https://europe-west1-aiplatform.googleapis.com", cReg.baseURL)
	}

	if _, err := NewVertexClient(context.Background(), VertexClientOptions{HTTPClient: hc}); err == nil {
		t.Fatal("expected error when project is empty")
	}
}

func TestVertexClientGenerateJSONWithUsageAndThinking(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"candidates":[{
				"content":{
					"role":"model",
					"parts":[
						{"text":"internal chain of thought","thought":true},
						{"text":"{\"focus_summary\":"},
						{"text":"\"ok\"}"}
					]
				}
			}],
			"usageMetadata":{
				"promptTokenCount":120,
				"candidatesTokenCount":45,
				"thoughtsTokenCount":30,
				"totalTokenCount":195
			}
		}`))
	}))
	defer srv.Close()

	vc, err := NewVertexClient(context.Background(), VertexClientOptions{
		Project:       "ai-billing-proj",
		Location:      "global",
		Model:         "gemini-4-carbon",
		ThinkingLevel: "medium",
		HTTPClient:    srv.Client(),
		BaseURL:       srv.URL,
	})
	if err != nil {
		t.Fatalf("NewVertexClient: %v", err)
	}

	raw, stats, err := vc.GenerateJSONWithUsage(context.Background(), "sys prompt", "usr prompt", &Schema{Type: "OBJECT"})
	if err != nil {
		t.Fatalf("GenerateJSONWithUsage: %v", err)
	}
	wantPath := "/v1/projects/ai-billing-proj/locations/global/publishers/google/models/gemini-4-carbon:generateContent"
	if gotPath != wantPath {
		t.Fatalf("request path = %q, want %q", gotPath, wantPath)
	}
	if string(raw) != `{"focus_summary":"ok"}` {
		t.Fatalf("raw = %q, want {\"focus_summary\":\"ok\"}", raw)
	}
	if stats.PromptTokens != 120 || stats.CandidateTokens != 45 || stats.ThoughtsTokens != 30 || stats.TotalTokens != 195 {
		t.Fatalf("unexpected usage stats: %+v", stats)
	}
	genCfg, _ := gotBody["generationConfig"].(map[string]any)
	thinkCfg, _ := genCfg["thinkingConfig"].(map[string]any)
	if thinkCfg["thinkingLevel"] != "MEDIUM" {
		t.Fatalf("thinkingLevel = %v, want MEDIUM", thinkCfg["thinkingLevel"])
	}
}

func TestVertexClientRetryOn429_503_504AndTimeout(t *testing.T) {
	t.Run("retries 429 and 503 then succeeds", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			switch attempts.Add(1) {
			case 1:
				http.Error(w, "rate limited", http.StatusTooManyRequests)
			case 2:
				http.Error(w, "unavailable", http.StatusServiceUnavailable)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}]}`))
			}
		}))
		defer srv.Close()

		vc, err := NewVertexClient(context.Background(), VertexClientOptions{
			Project:        "p1",
			RetryBaseDelay: 5 * time.Millisecond,
			HTTPClient:     srv.Client(),
			BaseURL:        srv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := vc.GenerateJSON(context.Background(), "sys", "usr", &Schema{Type: "OBJECT"})
		if err != nil {
			t.Fatalf("expected retry success, got %v", err)
		}
		if string(raw) != `{"ok":true}` || attempts.Load() != 3 {
			t.Fatalf("got raw=%s attempts=%d", raw, attempts.Load())
		}
	})

	t.Run("retries timeout and 504 then succeeds", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			switch attempts.Add(1) {
			case 1:
				time.Sleep(120 * time.Millisecond)
				http.Error(w, "slow", http.StatusOK)
			case 2:
				http.Error(w, "timeout", http.StatusGatewayTimeout)
			default:
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"{\"ok\":true}"}]}}]}`))
			}
		}))
		defer srv.Close()

		vc, err := NewVertexClient(context.Background(), VertexClientOptions{
			Project:        "p1",
			AttemptTimeout: 50 * time.Millisecond,
			RetryBaseDelay: 5 * time.Millisecond,
			HTTPClient:     srv.Client(),
			BaseURL:        srv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := vc.GenerateJSON(context.Background(), "sys", "usr", &Schema{Type: "OBJECT"})
		if err != nil {
			t.Fatalf("expected timeout+504 retry success, got %v", err)
		}
		if string(raw) != `{"ok":true}` || attempts.Load() != 3 {
			t.Fatalf("got raw=%s attempts=%d", raw, attempts.Load())
		}
	})

	t.Run("does not retry HTTP 400", func(t *testing.T) {
		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			http.Error(w, "invalid schema", http.StatusBadRequest)
		}))
		defer srv.Close()

		vc, err := NewVertexClient(context.Background(), VertexClientOptions{
			Project:        "p1",
			RetryBaseDelay: 5 * time.Millisecond,
			HTTPClient:     srv.Client(),
			BaseURL:        srv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vc.GenerateJSON(context.Background(), "sys", "usr", &Schema{Type: "OBJECT"}); err == nil {
			t.Fatal("expected HTTP 400 error")
		}
		if attempts.Load() != 1 {
			t.Fatalf("expected 1 attempt on HTTP 400, got %d", attempts.Load())
		}
	})

	t.Run("exhausts retries cleanly on persistent timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			time.Sleep(120 * time.Millisecond)
		}))
		defer srv.Close()

		vc, err := NewVertexClient(context.Background(), VertexClientOptions{
			Project:        "p1",
			AttemptTimeout: 35 * time.Millisecond,
			RetryBaseDelay: 5 * time.Millisecond,
			HTTPClient:     srv.Client(),
			BaseURL:        srv.URL,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vc.GenerateJSON(context.Background(), "sys", "usr", &Schema{Type: "OBJECT"}); err == nil {
			t.Fatal("expected timeout exhaustion error")
		}
	})
}

func TestProbeVertexModel(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		if strings.HasSuffix(r.URL.Path, "/missing-model") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"name":"models/gemini-4-carbon"}`))
	}))
	defer srv.Close()

	err := probeVertexModelWithOptions(context.Background(), VertexClientOptions{
		Project:    "my-proj",
		Location:   "global",
		Model:      "gemini-4-carbon",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	})
	if err != nil {
		t.Fatalf("probeVertexModelWithOptions failed: %v", err)
	}
	wantPath := "/v1/projects/my-proj/locations/global/publishers/google/models/gemini-4-carbon"
	if gotMethod != http.MethodGet || gotPath != wantPath {
		t.Fatalf("probe = %s %s, want GET %s", gotMethod, gotPath, wantPath)
	}

	err = probeVertexModelWithOptions(context.Background(), VertexClientOptions{
		Project:    "my-proj",
		Location:   "global",
		Model:      "missing-model",
		HTTPClient: srv.Client(),
		BaseURL:    srv.URL,
	})
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 probe error, got %v", err)
	}
}
