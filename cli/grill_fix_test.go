package cli

// Regression tests for grill findings (2026-10-10):
//   BUG 1 (run -07): temperature: 0 dropped on the wire by omitempty — the
//     temperatureZeroTransport must inject it into chat-completion bodies.
//   BUG 2 (run -04): chunkString emitted over-budget pieces for escape-heavy
//     strings (see jev_chunk_test.go for the regression test).

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func TestTemperatureZeroInjectedOnWire(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","created":0,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"{\"shouldkeep\":true,\"explanation\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()
	t.Setenv("JQLM_PROVIDER", "openai-compat")
	t.Setenv("JQLM_BASE_URL", srv.URL)
	t.Setenv("JQLM_API_KEY", "k")
	t.Setenv("JQLM_MODEL", "m")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	resetLLMDecider(t)
	d, err := getDecider()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.decide(map[string]any{"text": "zebra"}, "is a zebra mentioned?"); err != nil {
		t.Fatal(err)
	}
	temp, present := gotBody["temperature"]
	if !present {
		t.Fatal("temperature missing from wire body — omitempty still dropping it")
	}
	if tf, ok := temp.(float64); !ok || tf != 0 {
		t.Errorf("temperature on wire = %v, want 0", temp)
	}
}

func TestTemperatureZeroTransportNoDoubleInject(t *testing.T) {
	// bodies that already carry a temperature must pass through untouched
	tr := &temperatureZeroTransport{next: http.DefaultTransport}
	body := `{"model":"m","temperature":0.7,"messages":[]}`
	req, _ := http.NewRequest("POST", "http://x/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	captured := &bytes.Buffer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		captured.Write(b)
	}))
	defer srv.Close()
	req.URL = mustParseURL(t, srv.URL+"/v1/chat/completions")
	if _, err := tr.RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(captured.String(), "0.7") || strings.Count(captured.String(), "temperature") != 1 {
		t.Errorf("existing temperature modified: %s", captured.String())
	}
}

func resetLLMDecider(t *testing.T) {
	t.Helper()
	llmOnce = sync.Once{}
	llmShared = nil
	llmErr = nil
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
