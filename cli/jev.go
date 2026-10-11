// jev.go — Jev (TypeSafe AI "System One" decision model) builtins for jqlm.
//
// Adds three builtins to the query language:
//
//	jev_select(value; prompt)            -> boolean  (true = keep)
//	jev_judge(value; prompt)             -> {"keep": boolean, "noul": number}
//	jev_classify(value; prompt; opts...) -> string   (chosen option name)
//
// jev_select/jev_judge map to a Jev "noul" (yes/no) question; keep is decided
// by the returned yes-probability versus JEV_THRESHOLD. jev_classify maps to a
// "choice" question; each option argument is "name" or "name|description".
//
// Unlike the llm_* builtins (one chat-completion per item), Jev is a
// discriminative decision API: it never generates text, returns calibrated
// probabilities, and answers in ~70–500ms at a fraction of LLM cost. The llm
// path remains first-class for ambiguous criteria and adversarial corpora.
//
// Failed calls fail open, exactly like llm_*: the item is DISCARDED, a
// warning goes to stderr, and the run continues (with an end-of-run summary).
//
// Configuration (precedence: flags > env > config file > defaults):
//
//	JEV_API_KEY          API key (required) — never logged
//	JEV_BASE_URL         default https://api.typesafe.ai/v1
//	JEV_MODEL            default jev-latest (see GET /v1/models; jev-preview)
//	JEV_THRESHOLD        keep threshold for jev_select/jev_judge (default 0.5)
//	JEV_TIMEOUT          per-call timeout, Go duration (default 15s)
//	JEV_MAX_RETRIES      retries per call (default 3)
//	JEV_MAX_ITEM_BYTES   chunk budget (default 64KB, 0 = off). Jev's real
//	                     context window is much smaller than 1MB: a 76KB doc
//	                     passed but 296KB returned max_tokens_exceeded.
//
// API shape (verified against https://api.typesafe.ai/openapi.json, 2026-10):
//
//	POST {base}/systemone
//	{"model": "jev-latest",
//	 "state": <string|object|array>,
//	 "questions": {"keep": {"type": "noul", "instructions": "..."},
//	               "class": {"type": "choice", "instructions": "...",
//	                         "criteria": {"name": "description", ...}}}}
//	→ {"model": "...", "answers": {"keep": {"type": "noul", "noul": 0.93},
//	                                "class": {"type": "choice", "choice": "x",
//	                                          "confidence": 0.9}},
//	   "usage": {"input_tokens": N, "output_tokens": N}}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
	"sync/atomic"
	"time"
)

// ----------------------------------------------------------------- config ---

const (
	jevDefaultBaseURL  = "https://api.typesafe.ai/v1"
	jevDefaultModel    = "jev-latest"
	jevDefaultTimeout  = 15 * time.Second
	jevDefaultRetries  = 3
	jevDefaultMaxBytes = 64 << 10
)

type jevConfig struct {
	BaseURL      string
	Model        string
	Threshold    float64
	Timeout      time.Duration
	MaxRetries   int
	MaxItemBytes int
	APIKey       string
}

// jevFlags are set by the CLI flags before the decider is lazily constructed;
// they take precedence over env vars and the config file.
var (
	jevFlagModel     string
	jevFlagModelSet  bool
	jevFlagThreshold string
	jevShowConfig    bool // --jev-config: print settings, suppress startup warning
)

func jevLoadConfig() (c jevConfig, err error) {
	cfg := llmLoadConfig() // same config file (~/.config/jqlm/config.yaml)

	c = jevConfig{
		BaseURL:      jevDefaultBaseURL,
		Model:        jevDefaultModel,
		Threshold:    0.5,
		Timeout:      jevDefaultTimeout,
		MaxRetries:   jevDefaultRetries,
		MaxItemBytes: jevDefaultMaxBytes,
	}

	if cfg != nil {
		if cfg.JevBaseURL != "" {
			c.BaseURL = cfg.JevBaseURL
		}
		if cfg.JevModel != "" {
			c.Model = cfg.JevModel
		}
		if cfg.JevThreshold != nil {
			c.Threshold = *cfg.JevThreshold
		}
		if cfg.JevTimeout != "" {
			if d, e := time.ParseDuration(cfg.JevTimeout); e == nil {
				c.Timeout = d
			}
		}
		if cfg.JevMaxItemBytes != nil {
			c.MaxItemBytes = *cfg.JevMaxItemBytes
		}
	}
	if s := os.Getenv("JEV_BASE_URL"); s != "" {
		c.BaseURL = s
	}
	if s := os.Getenv("JEV_MODEL"); s != "" {
		c.Model = s
	}
	if s := os.Getenv("JEV_THRESHOLD"); s != "" {
		if f, e := strconv.ParseFloat(s, 64); e == nil {
			c.Threshold = f
		} else {
			return c, fmt.Errorf("invalid JEV_THRESHOLD %q", s)
		}
	}
	if s := os.Getenv("JEV_TIMEOUT"); s != "" {
		d, e := time.ParseDuration(s)
		if e != nil {
			return c, fmt.Errorf("invalid JEV_TIMEOUT %q: %v", s, e)
		}
		c.Timeout = d
	}
	if s := os.Getenv("JEV_MAX_RETRIES"); s != "" {
		if n, e := strconv.Atoi(s); e == nil && n > 0 {
			c.MaxRetries = n
		}
	}
	if s := os.Getenv("JEV_MAX_ITEM_BYTES"); s != "" {
		n, e := strconv.Atoi(s)
		if e != nil || n < 0 {
			return c, fmt.Errorf("invalid JEV_MAX_ITEM_BYTES %q", s)
		}
		c.MaxItemBytes = n
	}
	// flags last (highest precedence)
	if jevFlagModelSet {
		c.Model = jevFlagModel
	}
	if jevFlagThreshold != "" {
		f, e := strconv.ParseFloat(jevFlagThreshold, 64)
		if e != nil || f < 0 || f > 1 {
			return c, fmt.Errorf("invalid --jev-threshold %q (must be a number in [0,1])", jevFlagThreshold)
		}
		c.Threshold = f
	}

	c.APIKey = os.Getenv("JEV_API_KEY")
	if c.APIKey == "" {
		return c, fmt.Errorf("jev requires JEV_API_KEY (get a key from https://thejevai.com)")
	}
	if c.Threshold < 0 || c.Threshold > 1 {
		return c, fmt.Errorf("invalid JEV_THRESHOLD %v (must be in [0,1])", c.Threshold)
	}
	return c, nil
}

// ----------------------------------------------------------------- decider --

type jevDecider struct {
	baseURL      string
	model        string
	threshold    float64
	timeout      time.Duration
	maxRetries   int
	maxItemBytes int
	apiKey       string
	httpClient   *http.Client

	calls    int64
	failures int64
	uses     int64 // input tokens, summed
}

var (
	jevOnce   sync.Once
	jevShared *jevDecider
	jevErr    error
)

func getJevDecider() (*jevDecider, error) {
	jevOnce.Do(func() {
		cfg, err := jevLoadConfig()
		if err != nil {
			jevErr = err
			return
		}
		jevShared = &jevDecider{
			baseURL:      strings.TrimRight(cfg.BaseURL, "/"),
			model:        cfg.Model,
			threshold:    cfg.Threshold,
			timeout:      cfg.Timeout,
			maxRetries:   cfg.MaxRetries,
			maxItemBytes: cfg.MaxItemBytes,
			apiKey:       cfg.APIKey,
			httpClient:   &http.Client{},
		}
		if !llmShowConfig && !jevShowConfig {
			llmWarnf("jqlm: jev model=%s threshold=%.2f timeout=%s base_url=%s", cfg.Model, cfg.Threshold, cfg.Timeout, cfg.BaseURL)
		}
	})
	return jevShared, jevErr
}

// jevState converts a jq value into Jev "state": strings, objects, and arrays
// pass through directly (the API accepts all three); anything else is sent as
// its JSON string form.
func jevState(value any) any {
	switch value.(type) {
	case string, map[string]any, []any, nil:
		return value
	default:
		b, err := json.Marshal(value)
		if err != nil {
			return fmt.Sprintf("%v", value)
		}
		return string(b)
	}
}

// jevInputError checks whether the value is an input-decode error (invalid
// JSON/YAML input that reached the query as a value instead of aborting).
// Judging the error's text as if it were the document silently poisons
// results, so these must fail loudly.
func jevInputError(value any) error {
	if _, ok := value.(error); ok {
		return fmt.Errorf("input is not a document but an error value (%v) — fix the input JSON", value)
	}
	return nil
}

type jevQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions,omitempty"`
	Criteria     any    `json:"criteria,omitempty"`
}

type jevRequest struct {
	Model     string                 `json:"model"`
	State     any                    `json:"state"`
	Questions map[string]jevQuestion `json:"questions"`
}

type jevAnswer struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul"`
	Choice        *string            `json:"choice"`
	Confidence    *float64           `json:"confidence"`
	Probabilities map[string]float64 `json:"probabilities"`
}

type jevResponse struct {
	Model   string               `json:"model"`
	Answers map[string]jevAnswer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// jevCall posts one request and decodes the response. It returns the raw
// answers map; retry/status handling lives here too.
func (d *jevDecider) jevCall(req jevRequest) (map[string]jevAnswer, error) {
	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		atomic.AddInt64(&d.calls, 1)
		body, err := json.Marshal(req)
		if err != nil {
			return nil, err
		}
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/systemone", bytes.NewReader(body))
		if err != nil {
			cancel()
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+d.apiKey)

		resp, err := d.httpClient.Do(httpReq)
		if err != nil {
			cancel()
			lastErr = err
			if jevRetryable(err) {
				time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
				continue
			}
			return nil, err
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		if err != nil {
			lastErr = err
			time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("jev api %d: %s", resp.StatusCode, jevTrimBody(data))
			if resp.StatusCode == http.StatusTooManyRequests ||
				(resp.StatusCode >= 500 && resp.StatusCode < 600) {
				time.Sleep(time.Duration(200*(1<<attempt)) * time.Millisecond)
				continue
			}
			return nil, lastErr
		}
		var out jevResponse
		if err := json.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("jev: invalid response body: %v", err)
		}
		atomic.AddInt64(&d.uses, int64(out.Usage.InputTokens))
		return out.Answers, nil
	}
	return nil, lastErr
}

func jevRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return true // transport-level failures: retry
}

func jevTrimBody(data []byte) string {
	s := strings.TrimSpace(string(data))
	// validation errors echo the request back — never forward that to stderr
	if i := strings.Index(s, `"input"`); i > 0 {
		s = s[:i] + "…"
	}
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// chunkStates splits an oversized value into pieces that each fit the
// maxItemBytes budget (marshaled), so classification can OR-aggregate over
// chunks instead of dropping the item. Strings split at paragraph → line →
// rune boundaries; arrays batch elements; objects batch top-level entries
// (each batch entry is a single-key map, preserving key context). Returns the
// chunk list and whether chunking actually happened.
func (d *jevDecider) chunkStates(value any) ([]any, bool, error) {
	state := jevState(value)
	if d.maxItemBytes <= 0 {
		return []any{state}, false, nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return nil, false, err
	}
	if len(b) <= d.maxItemBytes {
		return []any{state}, false, nil
	}
	var chunks []any
	switch v := state.(type) {
	case string:
		chunks = chunkString(v, d.maxItemBytes)
	case []any:
		chunks, err = batchElements(v, d.maxItemBytes)
		if err != nil {
			return nil, false, err
		}
	case map[string]any:
		chunks, err = batchObject(v, d.maxItemBytes)
		if err != nil {
			return nil, false, err
		}
	default:
		// scalars can't realistically exceed the limit; fall through to one
		// state and let the normal path handle (or the API reject) it
		return []any{state}, false, nil
	}
	if len(chunks) <= 1 {
		return []any{state}, false, nil
	}
	llmWarnf("jqlm: item is %d bytes (JEV_MAX_ITEM_BYTES=%d), chunked into %d pieces for classification", len(b), d.maxItemBytes, len(chunks))
	return chunks, true, nil
}

// chunkString splits s into pieces whose json.Marshal form fits budget bytes,
// preferring paragraph breaks, then line breaks, then hard rune cuts.
func chunkString(s string, budget int) []any {
	// JSON-encoding overhead (quotes + escaping) can expand control chars 6x;
	// use a conservative inner budget (half of budget) and verify with a real
	// marshal per piece in the caller.
	inner := budget / 2
	if inner > len(s) {
		inner = len(s)
	}
	var out []any
	for len(s) > 0 {
		cut := len(s)
		if cut > inner {
			cut = inner
			// prefer the last paragraph break, then line break, then space
			for _, sep := range []string{"\n\n", "\n", " "} {
				if i := strings.LastIndex(s[:cut], sep); i > 0 {
					cut = i + len(sep)
					break
				}
			}
			// never split a UTF-8 rune
			for cut > 0 && !utf8.RuneStart(s[cut]) {
				cut--
			}
		}
		piece := strings.TrimRight(s[:cut], "\n")
		if piece != "" {
			out = append(out, piece)
		}
		s = s[cut:]
	}
	return out
}

// batchElements groups array elements into batches whose marshaled form fits
// budget bytes.
func batchElements(elems []any, budget int) ([]any, error) {
	var batches []any
	var cur []any
	curLen := 2 // "[]"
	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
			cur, curLen = nil, 2
		}
	}
	for _, el := range elems {
		eb, err := json.Marshal(el)
		if err != nil {
			return nil, err
		}
		if len(eb)+1 > budget { // single element too big: split string states inside
			flush()
			if s, ok := el.(string); ok {
				for _, piece := range chunkString(s, budget) {
					batches = append(batches, []any{piece})
				}
			} else {
				batches = append(batches, []any{el}) // let the API reject it
			}
			continue
		}
		if curLen+len(eb)+1 > budget {
			flush()
		}
		cur = append(cur, el)
		curLen += len(eb) + 1
	}
	flush()
	return batches, nil
}

// batchObject groups top-level object entries into batch objects whose
// marshaled form fits budget bytes. Each entry is emitted as a single-key
// object so the chunk keeps its key context.
func batchObject(obj map[string]any, budget int) ([]any, error) {
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var batches []any
	cur := map[string]any{}
	curLen := 2
	flush := func() {
		if len(cur) > 0 {
			batches = append(batches, cur)
			cur = map[string]any{}
			curLen = 2
		}
	}
	for _, k := range keys {
		entry := map[string]any{k: obj[k]}
		eb, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if len(eb) > budget { // single entry too big: split string values inside
			flush()
			if s, ok := obj[k].(string); ok {
				for _, piece := range chunkString(s, budget) {
					batches = append(batches, map[string]any{k: piece})
				}
			} else {
				batches = append(batches, entry) // let the API reject it
			}
			continue
		}
		if curLen+len(eb)+1 > budget {
			flush()
		}
		for kk, vv := range entry {
			cur[kk] = vv
		}
		curLen += len(eb) + 1
	}
	flush()
	return batches, nil
}

// decideNoul asks one yes/no question about the value. On failure it fails
// open (keep=false) and returns a non-empty reason; the error is never
// propagated as a query error, mirroring llm_* semantics. Oversized items are
// auto-chunked; keep = any chunk's noul >= threshold (max noul reported).
func (d *jevDecider) decideNoul(value any, prompt any) (keep bool, noul float64, reason string) {
	if err := jevInputError(value); err != nil {
		return false, 0, err.Error()
	}
	promptStr, ok := prompt.(string)
	if !ok {
		return false, 0, "jev prompt must be a string"
	}
	chunks, _, err := d.chunkStates(value)
	if err != nil {
		return false, 0, "marshal error"
	}
	maxNoul := 0.0
	for _, chunk := range chunks {
		answers, err := d.jevCall(jevRequest{
			Model: d.model,
			State: chunk,
			Questions: map[string]jevQuestion{
				"keep": {Type: "noul", Instructions: promptStr},
			},
		})
		if err != nil {
			atomic.AddInt64(&d.failures, 1)
			llmWarnf("jqlm: jev_select failed, item DISCARDED: %v", err)
			markFailedItem("jev_select", value, err.Error())
			return false, 0, err.Error()
		}
		ans, ok := answers["keep"]
		if !ok || ans.Noul == nil {
			atomic.AddInt64(&d.failures, 1)
			llmWarnf("jqlm: jev response missing noul verdict, item DISCARDED")
			return false, 0, "response omitted required field 'noul'"
		}
		if *ans.Noul > maxNoul {
			maxNoul = *ans.Noul
		}
	}
	noul = maxNoul
	return noul >= d.threshold, noul, ""
}

// decideChoice asks one choice question and returns the chosen option name.
// On failure it returns "" (falsy in jq) plus a non-empty reason.
func (d *jevDecider) decideChoice(value any, prompt any, options []any) (choice string, confidence float64, reason string) {
	if err := jevInputError(value); err != nil {
		return "", 0, err.Error()
	}
	promptStr, ok := prompt.(string)
	if !ok {
		return "", 0, "jev prompt must be a string"
	}
	if len(options) == 0 {
		return "", 0, "jev_classify requires at least one option"
	}
	criteria := make(map[string]string, len(options))
	for _, opt := range options {
		s, ok := opt.(string)
		if !ok {
			return "", 0, "jev_classify options must be strings"
		}
		if name, desc, found := strings.Cut(s, "|"); found {
			name, desc = strings.TrimSpace(name), strings.TrimSpace(desc)
			if name == "" {
				return "", 0, fmt.Sprintf("jev_classify option %q has an empty name", s)
			}
			criteria[name] = desc
		} else {
			criteria[s] = ""
		}
	}
	// Oversized items are auto-chunked; the verdict is OR-aggregated: per
	// option, the max probability seen in any chunk wins, argmax picks the
	// class (so "matches in any part of the document" is preserved).
	chunks, _, err := d.chunkStates(value)
	if err != nil {
		return "", 0, "marshal error"
	}
	bestProb := map[string]float64{}
	for _, chunk := range chunks {
		answers, err := d.jevCall(jevRequest{
			Model: d.model,
			State: chunk,
			Questions: map[string]jevQuestion{
				"class": {Type: "choice", Instructions: promptStr, Criteria: criteria},
			},
		})
		if err != nil {
			atomic.AddInt64(&d.failures, 1)
			llmWarnf("jqlm: jev_classify failed, returning null: %v", err)
			markFailedItem("jev_classify", value, err.Error())
			return "", 0, err.Error()
		}
		ans, ok := answers["class"]
		if !ok || ans.Choice == nil {
			atomic.AddInt64(&d.failures, 1)
			llmWarnf("jqlm: jev response missing choice verdict, returning null")
			return "", 0, "response omitted required field 'choice'"
		}
		// record the chosen option's evidence; prefer the full probability
		// distribution when the API provides one so runner-up options aren't lost
		if len(ans.Probabilities) > 0 {
			for opt, p := range ans.Probabilities {
				if p > bestProb[opt] {
					bestProb[opt] = p
				}
			}
		} else {
			p := 1.0
			if ans.Confidence != nil {
				p = *ans.Confidence
			}
			if p > bestProb[*ans.Choice] {
				bestProb[*ans.Choice] = p
			}
		}
	}
	// argmax over accumulated per-option max probabilities (deterministic:
	// ties broken by option name)
	choice, confidence = "", 0.0
	opts := make([]string, 0, len(bestProb))
	for opt := range bestProb {
		opts = append(opts, opt)
	}
	sort.Strings(opts)
	for _, opt := range opts {
		if bestProb[opt] > confidence {
			choice, confidence = opt, bestProb[opt]
		}
	}
	if choice == "" {
		return "", 0, "no chunk produced a verdict"
	}
	return choice, confidence, ""
}

// ---------------------------------------------------------------- builtins --

// jevSelectFunc implements jev_select(value; prompt) -> boolean.
func jevSelectFunc(_ any, args []any) any {
	if len(args) != 2 {
		return fmt.Errorf("jev_select(value; prompt): got %d args", len(args))
	}
	d, err := getJevDecider()
	if err != nil {
		return err
	}
	keep, _, _ := d.decideNoul(args[0], args[1])
	return keep
}

// jevJudgeFunc implements jev_judge(value; prompt) -> {keep, noul}.
func jevJudgeFunc(_ any, args []any) any {
	if len(args) != 2 {
		return fmt.Errorf("jev_judge(value; prompt): got %d args", len(args))
	}
	d, err := getJevDecider()
	if err != nil {
		return err
	}
	keep, noul, reason := d.decideNoul(args[0], args[1])
	out := map[string]any{"keep": keep, "noul": noul}
	if reason != "" {
		out["reason"] = reason
	}
	return out
}

// jevClassifyFunc implements jev_classify(value; prompt; options) -> string.
// options is a single jq array; each element is "name" or "name|description".
// Failed calls return null (falsy) with a stderr warning.
//
// Note: gojq invokes custom functions once per combination of argument
// values, so a variadic option list would fire one API call per option.
// Passing the options as one array argument guarantees a single call.
func jevClassifyFunc(_ any, args []any) any {
	if len(args) != 3 {
		return fmt.Errorf("jev_classify(value; prompt; options): got %d args", len(args))
	}
	d, err := getJevDecider()
	if err != nil {
		return err
	}
	options, ok := args[2].([]any)
	if !ok {
		return fmt.Errorf("jev_classify options must be an array (e.g. [\"billing\", \"technical\"])")
	}
	choice, _, _ := d.decideChoice(args[0], args[1], options)
	if choice == "" {
		return nil // null on failure, like other fail-open discard paths
	}
	return choice
}

// jevSummaryWarn prints an end-of-run summary if any jev calls failed.
func jevSummaryWarn() {
	if jevShared != nil {
		if calls, failures := atomic.LoadInt64(&jevShared.calls), atomic.LoadInt64(&jevShared.failures); failures > 0 {
			llmWarnf("jqlm: %d of %d jev call(s) failed; those items were DISCARDED", failures, calls)
		}
	}
}

// jevRedactKey shows only the head and tail of an api key so --jev-config
// output can never leak the secret half.
func jevRedactKey(key string) string {
	if len(key) <= 12 {
		return "(redacted)"
	}
	return key[:8] + "…" + key[len(key)-4:]
}
