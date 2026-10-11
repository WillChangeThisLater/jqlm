// jev_test.go — unit tests for the pure helpers in cli/jev.go (no network).

package cli

import (
	"strings"
	"testing"
	"time"
)

func TestJevState(t *testing.T) {
	// strings, objects, and arrays pass through unchanged (API-native shapes)
	if got := jevState("hello"); got != "hello" {
		t.Errorf("string state: got %#v", got)
	}
	obj := map[string]any{"a": 1}
	got := jevState(obj)
	m, ok := got.(map[string]any)
	if !ok || m["a"] != 1 {
		t.Errorf("object state: got %#v", got)
	}
	arr := []any{1, 2}
	got = jevState(arr)
	s, ok := got.([]any)
	if !ok || len(s) != 2 {
		t.Errorf("array state: got %#v", got)
	}
	// numbers/bools become their JSON string form
	if got := jevState(42); got != "42" {
		t.Errorf("number state: got %#v", got)
	}
	if got := jevState(true); got != "true" {
		t.Errorf("bool state: got %#v", got)
	}
}

func TestJevTrimBody(t *testing.T) {
	// validation errors echo the request back — must be stripped before
	// the body lands in stderr (defense against reflected sensitive state)
	body := `{"detail":[{"msg":"Field required","input":{"secret":"hunt4treasure","more":"xxx"}}]}`
	got := jevTrimBody([]byte(body))
	if strings.Contains(got, "hunt4treasure") {
		t.Errorf("reflected input leaked: %s", got)
	}
	if !strings.Contains(got, "…") {
		t.Errorf("expected truncation marker: %s", got)
	}
	// long bodies get capped
	long := strings.Repeat("x", 1000)
	if got := jevTrimBody([]byte(long)); len(got) > 310 { // 300 chars + 3-byte ellipsis
		t.Errorf("long body not capped: %d", len(got))
	}
}

func TestJevRedactKey(t *testing.T) {
	key := "apikey_298988b80dff99340d49d4772b62e2a79ee_5463394a6937f3f924984b3ee0168980ce9c7c9192d3dc8a1dbc49583d4bbc81"
	got := jevRedactKey(key)
	if got != "apikey_2…bc81" {
		t.Errorf("redact: got %q", got)
	}
	if got := jevRedactKey("short"); got != "(redacted)" {
		t.Errorf("short key redact: got %q", got)
	}
	if jevRedactKey(key) == key {
		t.Error("full key must never be emitted")
	}
}

func TestJevConfigDefaults(t *testing.T) {
	t.Setenv("JEV_API_KEY", "k")
	jevFlagModel, jevFlagModelSet, jevFlagThreshold = "", false, ""
	c, err := jevLoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.BaseURL != "https://api.typesafe.ai/v1" || c.Model != "jev-latest" ||
		c.Threshold != 0.5 || c.Timeout != 15*time.Second || c.MaxRetries != 3 ||
		c.MaxItemBytes != 64<<10 {
		t.Errorf("unexpected defaults: %+v", c)
	}
}

func TestJevConfigThresholdValidation(t *testing.T) {
	t.Setenv("JEV_API_KEY", "k")
	jevFlagModel, jevFlagModelSet = "", false
	jevFlagThreshold = "1.5"
	if _, err := jevLoadConfig(); err == nil {
		t.Error("threshold 1.5 must be rejected")
	}
	jevFlagThreshold = "0.75"
	c, err := jevLoadConfig()
	if err != nil || c.Threshold != 0.75 {
		t.Errorf("threshold 0.75: got %+v err=%v", c, err)
	}
	jevFlagThreshold = ""
	t.Setenv("JEV_THRESHOLD", "notanumber")
	if _, err := jevLoadConfig(); err == nil {
		t.Error("invalid JEV_THRESHOLD must be rejected")
	}
}

func TestJevConfigRequiresKey(t *testing.T) {
	t.Setenv("JEV_API_KEY", "")
	jevFlagModel, jevFlagModelSet, jevFlagThreshold = "", false, ""
	if _, err := jevLoadConfig(); err == nil {
		t.Error("missing JEV_API_KEY must be rejected")
	}
}

func TestJevConfigFlagOverrides(t *testing.T) {
	t.Setenv("JEV_API_KEY", "k")
	t.Setenv("JEV_MODEL", "jev-preview")
	jevFlagModel, jevFlagModelSet, jevFlagThreshold = "jev-latest", true, ""
	c, err := jevLoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "jev-latest" {
		t.Errorf("flag must beat env: got %q", c.Model)
	}
	jevFlagModel, jevFlagModelSet = "", false
	c, err = jevLoadConfig()
	if err != nil || c.Model != "jev-preview" {
		t.Errorf("env model: got %q err=%v", c.Model, err)
	}
}

func TestWarnCoalescing(t *testing.T) {
	// fresh state (tests may run concurrently with nothing else touching these)
	warnMu.Lock()
	warnSeen = map[string]bool{}
	warnRepeated = map[string]int{}
	warnMu.Unlock()
	llmWarnf("dup warning: %d", 1)
	llmWarnf("dup warning: %d", 1)
	llmWarnf("dup warning: %d", 1)
	llmWarnf("other warning")
	warnMu.Lock()
	defer warnMu.Unlock()
	if got := warnRepeated["dup warning: 1"]; got != 2 {
		t.Errorf("repeated count: got %d, want 2", got)
	}
	if _, seen := warnSeen["other warning"]; !seen {
		t.Error("unique warning not recorded")
	}
	if len(warnRepeated) != 1 {
		t.Errorf("unique warnings must not be counted as repeated: %v", warnRepeated)
	}
}

func TestTotalFailedCallsAndMarkFailed(t *testing.T) {
	markFailed := llmMarkFailed
	llmMarkFailed = true
	defer func() { llmMarkFailed = markFailed }()
	// markFailedItem must not panic on unmarshalable values
	markFailedItem("jev_select", func() {}, "test error")
	if totalFailedCalls() != 0 {
		t.Errorf("no calls made, expected 0 failures, got %d", totalFailedCalls())
	}
}
