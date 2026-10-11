package cli

// jev_integration_test.go — end-to-end tests against the real Jev API.
//
// These verify that verdicts are *reasonable*, not just that plumbing types
// line up: unit tests cover chunk shapes and aggregation logic, but only the
// live API can catch "the model stopped understanding English" or
// "verdicts are garbage" regressions (both of which have happened — see the
// parse-error poisoning bug fixed 2026-10-10).
//
// Gated: skipped unless JEV_INTEGRATION=1 (and a real JEV_API_KEY is
// available), so `go test ./...` stays hermetic and costs nothing.
//
// Run: JEV_INTEGRATION=1 go test ./cli/ -run TestJevIntegration -v
//
// Assertion policy: loose numeric bands (noul > 0.5 / < 0.5), never exact
// probabilities; classify asserts membership in the option set only. A truly
// deterministic corpus wouldn't survive model updates, and flaky tests get
// ignored.

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

// resetJevDecider clears the process-wide singleton so per-test env vars
// (JEV_MAX_ITEM_BYTES etc.) actually take effect on the next call. White-box:
// in one real jqlm process config is read once; tests share the process.
func resetJevDecider(t *testing.T) {
	t.Helper()
	jevOnce = sync.Once{}
	jevShared = nil
	jevErr = nil
}

func TestJevIntegration(t *testing.T) {
	if os.Getenv("JEV_INTEGRATION") != "1" {
		t.Skip("set JEV_INTEGRATION=1 to run live-API tests (needs a real JEV_API_KEY)")
	}
	if os.Getenv("JEV_API_KEY") == "" {
		t.Skip("JEV_INTEGRATION=1 but no JEV_API_KEY available")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // hermetic config: ignore ~/.config/jqlm
	if err := os.Setenv("NO_COLOR", "1"); err != nil {
		t.Fatal(err)
	}

	runJq := func(t *testing.T, env map[string]string, query, input string) (stdout, stderr string) {
		t.Helper()
		resetJevDecider(t)
		for k, v := range env {
			t.Setenv(k, v)
		}
		var outStream, errStream strings.Builder
		c := cli{inStream: newStringReader(input), outStream: &outStream, errStream: &errStream}
		if code := c.run([]string{"-r", query}); code != 0 {
			t.Fatalf("jqlm exited %d; stderr: %s", code, errStream.String())
		}
		return outStream.String(), errStream.String()
	}

	judgeOut := func(t *testing.T, stdout string) (keep bool, noul float64) {
		t.Helper()
		var m struct {
			Keep bool    `json:"keep"`
			Noul float64 `json:"noul"`
		}
		if err := json.Unmarshal([]byte(stdout), &m); err != nil {
			t.Fatalf("unparseable judge output %q: %v", stdout, err)
		}
		return m.Keep, m.Noul
	}

	needleDoc := strings.Repeat("filler paragraph about gardening tips.\n\n", 30) +
		"the secret word is ZEBRA-CODE-42 appears here.\n\n" +
		strings.Repeat("more filler about accounting practices.\n\n", 30)

	t.Run("sanity_positive", func(t *testing.T) {
		// the plainest possible positive: if this regresses, every other
		// result from the API is suspect
		stdout, _ := runJq(t, nil,
			`jev_judge(. ; "is a zebra mentioned in this text?")`,
			`"we saw a ZEBRA at the zoo."`)
		keep, noul := judgeOut(t, stdout)
		if !keep || noul < 0.5 {
			t.Errorf("trivially-true judged false: keep=%v noul=%v — API or request format is broken", keep, noul)
		}
	})

	t.Run("sanity_negative", func(t *testing.T) {
		stdout, _ := runJq(t, nil,
			`jev_judge(. ; "is a zebra mentioned in this text?")`,
			`"tomatoes, soil, and compost are the heart of good gardening."`)
		keep, noul := judgeOut(t, stdout)
		if keep || noul > 0.5 {
			t.Errorf("trivially-false judged true: keep=%v noul=%v — verdicts are degenerate", keep, noul)
		}
	})

	t.Run("chunked_needle_found", func(t *testing.T) {
		// the feature's core contract: a needle past the first chunk, in an
		// item far over budget, is still found (OR-aggregation over chunks)
		stdout, stderr := runJq(t, map[string]string{"JEV_MAX_ITEM_BYTES": "300"},
			`jev_judge(. ; "does this text mention the secret word ZEBRA-CODE-42?")`,
			mustJSONString(t, needleDoc))
		if !strings.Contains(stderr, "chunked into") {
			t.Errorf("expected chunking warning on stderr, got: %s", stderr)
		}
		keep, noul := judgeOut(t, stdout)
		if !keep || noul < 0.5 {
			t.Errorf("chunked needle not found: keep=%v noul=%v — chunking or OR-aggregation is broken", keep, noul)
		}
	})

	t.Run("chunked_classify_member", func(t *testing.T) {
		stdout, _ := runJq(t, map[string]string{"JEV_MAX_ITEM_BYTES": "300"},
			`jev_classify(. ; "what is this document about?"; ["gardening|plants and soil", "accounting|money and ledgers", "other"])`,
			mustJSONString(t, needleDoc))
		choice := strings.TrimSpace(stdout)
		choice = strings.Trim(choice, "\"") // tolerate -r/-c output differences
		switch choice {
		case "gardening", "accounting", "other":
			// member of the option set — good enough; the doc is genuinely
			// mixed (30 gardening + 30 accounting paras) so don't pin one
		default:
			t.Errorf("classify returned %q, not a member of the option set (or null on failure)", choice)
		}
	})

	t.Run("poison_guard_raw_input", func(t *testing.T) {
		// raw non-JSON input must produce an explicit error-value reason, not
		// a confident garbage verdict (the 2026-10-10 poisoning bug)
		stdout, _ := runJq(t, map[string]string{"JEV_API_KEY": "k"},
			`jev_judge(. ; "is a zebra mentioned?")`,
			"we saw a ZEBRA at the zoo.\n")
		var m struct {
			Keep   bool   `json:"keep"`
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal([]byte(stdout), &m); err != nil {
			t.Fatalf("unparseable output %q: %v", stdout, err)
		}
		if m.Keep {
			t.Error("parse-error input judged keep=true — poisoning regression")
		}
		if !strings.Contains(m.Reason, "error value") {
			t.Errorf("expected explicit error-value reason, got: %s", m.Reason)
		}
	})
}

// mustJSONString marshals s as a JSON string literal for piped input.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
