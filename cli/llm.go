// llm.go — LLM filtering builtins for jqlm (a fork of gojq).
//
// Adds two builtins to the query language:
//
//	llm_select(value; prompt)  -> boolean  (true = keep)
//	llm_judge(value; prompt)   -> {"keep": boolean, "reason": string}
//
// Both send one request per item to an OpenAI-compatible chat completions
// endpoint. A failed call fails open: the item is DISCARDED, a warning goes
// to stderr, and the pipeline continues. End-of-run summary warns if any
// calls failed.
//
// Configuration is env-only (keeps this file out of upstream flag parsing):
//
//	JQLM_PROVIDER       openai | openrouter | llama | openai-compat (default openai)
//	JQLM_MODEL          model id (default depends on provider)
//	JQLM_BASE_URL       API base URL override
//	JQLM_MODE           json | json-schema | tool (default depends on provider)
//	JQLM_TIMEOUT        per-call timeout, Go duration (default depends on provider)
//	JQLM_MAX_RETRIES    retries per call (default 3)
//	JQLM_API_KEY        API key (overrides per-provider vars)
//
// Provider keys: OPENAI_API_KEY (openai), OPENROUTER_API_KEY (openrouter);
// llama needs no key; openai-compat uses JQLM_API_KEY.

package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	instructor "github.com/instructor-ai/instructor-go/pkg/instructor"
	openai "github.com/sashabaranov/go-openai"
)

// ---------------------------------------------------------------- providers --

type llmProviderSpec struct {
	name           string
	keyEnv         string // "" = no key needed
	defaultBaseURL string // "" = must be supplied via JQLM_BASE_URL
	defaultModel   string // "" = must be supplied via JQLM_MODEL
	defaultMode    instructor.Mode
	defaultTimeout time.Duration
	description    string
}

var llmProviders = map[string]llmProviderSpec{
	"openai": {
		name: "openai", keyEnv: "OPENAI_API_KEY",
		defaultBaseURL: "https://api.openai.com/v1", defaultModel: "gpt-4o-mini",
		defaultMode: instructor.ModeJSON, defaultTimeout: 8 * time.Second,
		description: "OpenAI API",
	},
	"openrouter": {
		name: "openrouter", keyEnv: "OPENROUTER_API_KEY",
		defaultBaseURL: "https://openrouter.ai/api/v1", defaultModel: "",
		defaultMode: instructor.ModeJSON, defaultTimeout: 15 * time.Second,
		description: "OpenRouter (any model id, e.g. z-ai/glm-5.3-flash)",
	},
	"llama": {
		name: "llama", keyEnv: "",
		defaultBaseURL: "http://localhost:8080/v1", defaultModel: "",
		defaultMode: instructor.ModeJSONSchema, defaultTimeout: 60 * time.Second,
		description: "local llama.cpp llama-server (grammar-constrained JSON)",
	},
	"openai-compat": {
		name: "openai-compat", keyEnv: "JQLM_API_KEY",
		defaultBaseURL: "", defaultModel: "",
		defaultMode: instructor.ModeJSON, defaultTimeout: 15 * time.Second,
		description: "any OpenAI-compatible endpoint; needs JQLM_BASE_URL and JQLM_MODEL (vLLM, LM Studio, Ollama, Groq, ...)",
	},
}

// ------------------------------------------------------------------ decider --

type llmDecision struct {
	Explanation string `json:"explanation,omitempty" jsonschema:"title=explanation,description=Short explanation for why this item should be kept or discarded"`
	ShouldKeep  bool   `json:"shouldkeep" jsonschema:"title=shouldkeep,description=Return true if we should keep the item, false if we should discard"`
}

type llmDecider struct {
	client       *instructor.InstructorOpenAI
	model        string
	provider     string
	maxRetries   int
	timeout      time.Duration
	maxItemBytes int
	calls        int64
	failures     int64
}

var (
	llmOnce   sync.Once
	llmShared *llmDecider
	llmErr    error
)

func getDecider() (*llmDecider, error) {
	llmOnce.Do(func() { llmShared, llmErr = newLLMDecider() })
	return llmShared, llmErr
}

func newLLMDecider() (*llmDecider, error) {
	name := os.Getenv("JQLM_PROVIDER")
	if name == "" {
		name = "openai"
	}
	spec, ok := llmProviders[name]
	if !ok {
		return nil, fmt.Errorf("unknown JQLM_PROVIDER %q (known: openai, openrouter, llama, openai-compat)", name)
	}

	key := os.Getenv("JQLM_API_KEY")
	if key == "" && spec.keyEnv != "" {
		key = os.Getenv(spec.keyEnv)
	}
	if spec.keyEnv != "" && key == "" {
		return nil, fmt.Errorf("provider %s requires %s (or JQLM_API_KEY)", spec.name, spec.keyEnv)
	}
	if spec.keyEnv == "" {
		key = "no-key-needed" // llama-server ignores auth
	}

	model := os.Getenv("JQLM_MODEL")
	if model == "" {
		model = spec.defaultModel
	}
	if model == "" {
		return nil, fmt.Errorf("provider %s requires JQLM_MODEL", spec.name)
	}

	baseURL := os.Getenv("JQLM_BASE_URL")
	if baseURL == "" {
		baseURL = spec.defaultBaseURL
	}
	if baseURL == "" {
		return nil, fmt.Errorf("provider %s requires JQLM_BASE_URL", spec.name)
	}

	mode := spec.defaultMode
	switch ms := os.Getenv("JQLM_MODE"); ms {
	case "":
	case "json":
		mode = instructor.ModeJSON
	case "json-schema":
		mode = instructor.ModeJSONSchema
	case "tool":
		mode = instructor.ModeToolCall
	default:
		return nil, fmt.Errorf("invalid JQLM_MODE %q (known: json, json-schema, tool)", ms)
	}

	timeout := spec.defaultTimeout
	if ts := os.Getenv("JQLM_TIMEOUT"); ts != "" {
		d, err := time.ParseDuration(ts)
		if err != nil {
			return nil, fmt.Errorf("invalid JQLM_TIMEOUT %q: %v", ts, err)
		}
		timeout = d
	}

	maxItemBytes := 0
	if ms := os.Getenv("JQLM_MAX_ITEM_BYTES"); ms != "" {
		n, err := strconv.Atoi(ms)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid JQLM_MAX_ITEM_BYTES %q", ms)
		}
		maxItemBytes = n
	}

	retries := 3
	if rs := os.Getenv("JQLM_MAX_RETRIES"); rs != "" {
		if n, err := strconv.Atoi(rs); err == nil && n > 0 {
			retries = n
		}
	}

	cfg := openai.DefaultConfig(key)
	cfg.BaseURL = baseURL
	cli := instructor.FromOpenAI(
		openai.NewClientWithConfig(cfg),
		instructor.WithMode(mode),
		instructor.WithMaxRetries(2),
	)
	llmWarnf("jqlm: provider=%s model=%s mode=%s timeout=%s maxItemBytes=%d", spec.name, model, mode, timeout, maxItemBytes)
	return &llmDecider{client: cli, model: model, provider: spec.name, maxRetries: retries, timeout: timeout, maxItemBytes: maxItemBytes}, nil
}

func (d *llmDecider) decide(value, prompt any) (keep bool, reason string, err error) {
	b, err := json.Marshal(value)
	if err != nil {
		return false, "marshal error", nil
	}
	promptStr, ok := prompt.(string)
	if !ok {
		return false, "", errors.New("llm prompt must be a string")
	}

	// instructor-go prepends its own user message with the JSON schema
	// instructions, so a system message would land mid-conversation — which
	// chat templates like Qwen's (llama.cpp) reject outright. Fold the judge
	// instruction into the user message for every provider instead.
	judge := "You are a strict JSON judge. Only output JSON that matches the provided schema."
	user := fmt.Sprintf("%s\n\nTask:\n%s\n\nData to evaluate (JSON):\n%s", judge, promptStr, string(b))

	// Client-side guard: some providers silently truncate oversized inputs
	// instead of erroring, which would produce a confidently wrong verdict.
	// Discard explicitly instead.
	if d.maxItemBytes > 0 && len(b) > d.maxItemBytes {
		atomic.AddInt64(&d.calls, 1)
		atomic.AddInt64(&d.failures, 1)
		llmWarnf("jqlm: item is %d bytes (JQLM_MAX_ITEM_BYTES=%d), DISCARDED without calling provider", len(b), d.maxItemBytes)
		return false, "item too large (client-side limit)", nil
	}
	messages := []openai.ChatCompletionMessage{
		{Role: openai.ChatMessageRoleUser, Content: user},
	}

	var out llmDecision
	var lastErr error
	for attempt := 0; attempt < d.maxRetries; attempt++ {
		atomic.AddInt64(&d.calls, 1)
		ctx, cancel := context.WithTimeout(context.Background(), d.timeout)
		_, err = d.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model: d.model, Messages: messages,
		}, &out)
		cancel()
		if err == nil {
			return out.ShouldKeep, out.Explanation, nil
		}
		lastErr = err
		if llmRetryable(err) {
			select {
			case <-time.After(time.Duration(200*(1<<attempt)) * time.Millisecond):
				continue
			}
		}
		break
	}
	atomic.AddInt64(&d.failures, 1)
	llmWarnf("jqlm: decide failed, item DISCARDED: %s", llmErrorSummary(lastErr))
	return false, llmErrorSummary(lastErr), nil
}

func llmRetryable(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return apiErr.HTTPStatusCode == http.StatusTooManyRequests ||
			(apiErr.HTTPStatusCode >= 500 && apiErr.HTTPStatusCode < 600)
	}
	return false
}

func llmErrorSummary(err error) string {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		if apiErr.HTTPStatusCode == 400 &&
			(strings.Contains(strings.ToLower(apiErr.Message), "maximum context length") ||
				strings.Contains(strings.ToLower(apiErr.Message), "context_length_exceeded") ||
				strings.Contains(strings.ToLower(apiErr.Message), "too large")) {
			return "input too large for model context"
		}
		return fmt.Sprintf("api %d: %s", apiErr.HTTPStatusCode, apiErr.Message)
	}
	return err.Error()
}

func llmWarnf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "warning: "+format+"\n", args...)
}

// ---------------------------------------------------------------- builtins --

// llmSelectFunc implements llm_select(value; prompt) -> boolean.
func llmSelectFunc(_ any, args []any) any {
	if len(args) != 2 {
		return fmt.Errorf("llm_select(value; prompt): got %d args", len(args))
	}
	d, err := getDecider()
	if err != nil {
		return err
	}
	keep, _, err := d.decide(args[0], args[1])
	if err != nil {
		return err
	}
	return keep
}

// llmJudgeFunc implements llm_judge(value; prompt) -> {keep, reason}.
func llmJudgeFunc(_ any, args []any) any {
	if len(args) != 2 {
		return fmt.Errorf("llm_judge(value; prompt): got %d args", len(args))
	}
	d, err := getDecider()
	if err != nil {
		return err
	}
	keep, reason, err := d.decide(args[0], args[1])
	if err != nil {
		return err
	}
	return map[string]any{"keep": keep, "reason": reason}
}

// llmSummaryWarn prints an end-of-run summary if any calls failed. Called
// after the query finishes; cheap no-op when everything succeeded.
func llmSummaryWarn() {
	if llmShared != nil {
		if calls, failures := atomic.LoadInt64(&llmShared.calls), atomic.LoadInt64(&llmShared.failures); failures > 0 {
			llmWarnf("jqlm: %d of %d llm call(s) failed; those items were DISCARDED", failures, calls)
		}
	}
}
