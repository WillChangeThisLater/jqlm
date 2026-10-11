---
name: jqlm
description: Use jqlm (a jq fork with llm_select/llm_judge/jev_select/jev_judge/jev_classify builtins) for semantic filtering of JSON streams — especially HAR network-capture recon of websites (authorized pen-testing, understanding how a site operates), log triage, and large-scale "which of these items matter" questions.
---

# jqlm — semantic filtering for JSON (HAR recon and friends)

`jqlm` is a fork of gojq (bug-for-bug jq in Go) with LLM builtins. Repo:
`~/repos/jqlm` (github: WillChangeThisLater/jqlm). Binary on PATH via
`~/.local/bin/jqlm` → `~/repos/jqlm/jqlm`. Read `~/repos/jqlm/README.md` and
`PROOF.md` for details.

## The builtins

```jq
llm_select(value; "prompt")   # -> bool: true = keep. Use inside select().
llm_judge(value; "prompt")    # -> {keep: bool, reason: string}

jev_select(value; "prompt")   # -> bool (Jev noul yes-probability >= --jev-threshold, default 0.5)
jev_judge(value; "prompt")    # -> {keep: bool, noul: number}  (raw probability for cascades)
jev_classify(value; "prompt"; ["opt|desc", ...])  # -> string: chosen option, null on failure

# Auto-chunking (2026-10-10): items over JEV_MAX_ITEM_BYTES are split into
# budget-sized chunks and verdicts OR-aggregated (max noul / max per-option
# probability) instead of being discarded. Strings split on paragraph → line
# → rune boundaries; arrays batch elements; objects batch single-key entries.
# Chunking = recall filter; cross-chunk criteria still need a full read.
```

One HTTP request per item. **Failed calls fail open: the item is DISCARDED
with a stderr warning.** Always glance at stderr for `N of M llm/jev call(s)
failed` — an outage silently shrinks output otherwise (by design; the warning
is the tell). For pipelines use `--fail-closed` (exit 6 on any failure) and
`--mark-failed` (discarded items emitted to stderr as NDJSON, recoverable).
Duplicate warnings are coalesced (`(seen Nx)` summary at end of run).
**Live-audit lesson (2026-10-07 bbc.com HAR run):** an empty-looking result
appeared TWICE — once from a broken key (401s, all 124 items silently
discarded) and once as a genuinely clean filter. Never trust an empty result
without checking for the failure summary; prefer `--fail-closed` in scripts.

### llm vs jev — DEFAULT TO JEV, escalate to llm

**Reach for `jev_select`/`jev_classify` first.** They are ~70–500ms per item
vs seconds for an LLM judge and 5–50× cheaper, with calibrated probabilities
and zero hallucinated output. For the bulk of jqlm's use cases — "which of
these items matter?" filtering, routing, tagging — jev is the right tool and
the cost difference compounds fast on large streams (HAR recon, log triage).

Escalate to `llm_select`/`llm_judge` only when:

- the criteria are **nuanced or compositional** ("keep only if the reviewer
  actually ATE the food and the complaint is about seasoning, not temperature")
  — freeform judge prompts handle this; jev questions should stay short and
  single-decision;
- the corpus is **adversarial** (prompt-injection risk, items engineered to
  look positive): an LLM judge's `reason` field doubles as tamper evidence,
  while jev gives you only a number;
- you need the verdict **explained** in the output (`llm_judge` returns it).

Quick test: if your prompt fits in one sentence with a clear yes/no or
pick-one answer space, use jev. If you're about to write a paragraph of
criteria, use llm.

- `jev_classify(value; "prompt"; ["opt|desc", ...])` has **no llm equivalent**
  (a Choice question with per-option probabilities) — for multi-way decisions
  it's the only one-call option, and it's the cheapest.
- jev state passes strings/objects/arrays natively; option args use
  `"name|description"`. NOTE: options MUST be one array argument — gojq calls
  custom functions once per combination of argument values, so comma-separated
  options would fire one API call per option.
- Cascade pattern for high-stakes filters: `jev_judge` low-confidence items
  fall through to `llm_judge` (the AY Automate benchmark matched frontier-LLM
  accuracy at ~27% of its cost this way). Threshold tune on real data first.

## Config / settings

- Defaults live in `~/.config/jqlm/config.yaml` (currently openrouter +
  z-ai/glm-5.3-flash, 30s timeout, concurrency 10).
- Check what would actually run: `jqlm --llm-config` (also validates config).
- Override per-invocation: `--llm-provider openai|openrouter|llama|openai-compat`,
  `--llm-model <id>`, `--llm-concurrency N`. Env `JQLM_*` also works.
- Local llama.cpp option: `--llm-provider llama --llm-model Qwen3.5-9B-Q8_0.gguf`
  (llama-server on :8080; start via `~/models/qwen3/run.sh`, add `-np 8
  --ctx-size 32768` for parallel slots; VRAM limits slots on a 16GB GPU).
- API keys: `OPENAI_API_KEY` / `OPENROUTER_API_KEY` env (llama needs none).

### Jev config

- Key: `JEV_API_KEY` in `~/.zshrc-sensitive` (verified working 2026-10-07).
- Defaults: `api.typesafe.ai/v1`, model `jev-latest`, threshold 0.5, 15s timeout.
- Flags `--jev-model` / `--jev-threshold <0..1>`; `--jev-config` prints resolved
  settings (key redacted). Config-file keys `jev_model`, `jev_threshold`, etc.
- `--llm-concurrency N` applies to jev calls too (6 items ran in 0.22s at N=6).
- Guardrail learned the hard way: after editing `~/repos/jqlm`, REBUILD the
  binary (`go build -o jqlm ./cmd/gojq`) — the PATH binary is a symlink to the
  repo and goes stale silently (a session lost an hour to "jev_select/2 is not
  defined" from a Sep 14 binary).

## Critical usage rules

1. **Parallelism is per input value.** A single big JSON array is ONE input =
   sequential. Pre-split: `jq -c '.[]' big.json | jqlm --llm-concurrency N
   '...'`. Output order is preserved; `jq -s` to rewrap.
1b. **In audit/pipeline scripts, always add `--fail-closed`.** Fail open means
   a provider outage is indistinguishable from "no matches" — exit 6 is the
   tell. Add `--mark-failed` if discarded items matter.
2. Big single documents (e.g. an entire 3MB HAR as one value) are handled
   fine but filtered sequentially — slice before filtering when speed matters.
3. Providers may **silently truncate** oversized items (OpenRouter does).
   Set `JQLM_MAX_ITEM_BYTES` (env or config) to discard explicitly with a
   warning instead of trusting a truncated verdict — llm_* builtins only.
   The jev_* builtins instead auto-chunk oversized items (see builtins
   section above).
3b. **Invalid JSON input poisons results (fixed 2026-10-10).** A raw-text
   stdin line used to reach jev_*/llm_* as a parse-error *value*, which was
   silently judged as if it were the document (noul 0.02-0.04 garbage
   verdicts, no failure warning). Now guarded: such items fail loudly with
   a reason and count toward the failure summary. When piping non-JSON
   text, wrap it first: `jq -Rs . file.txt | jqlm ...`.
4. Judge requests run at temperature 0 (deterministic).
5. `-r` for raw strings, `-c` for compact — same flags as jq.

## HAR recon workflow (authorized targets only — see guardrails)

Capture first (Chrome on an isolated Xvfb display via the x11-gui-automation +
browser skills, own CDP port, never someone else's browser):

```sh
~/.pi/agent/skills/x11-gui-automation/scripts/x11_env.sh claim harcap chrome
~/.pi/agent/skills/x11-gui-automation/scripts/x11_env.sh run harcap chrome \
  google-chrome --remote-debugging-port=9231 --user-data-dir=/tmp/chrome-har --no-first-run about:blank
python3 ~/repos/jqlm/skill/scripts/har_capture.py 9231 https://TARGET 60 /tmp/har.json
~/.pi/agent/skills/x11-gui-automation/scripts/x11_env.sh release harcap
# har_capture.py <cdp_port> <url> <seconds> <out.har> — includes response
# bodies (text, <=100KB each) as .response.content.text
```

Then filter. Always slice first, filter second:

```sh
jq -c '.log.entries[]' /tmp/har.json | jqlm --llm-concurrency 10 \
  'select(llm_select(. ; "<prompt>")) | ...'
```

Prompt recipes (tune exclusions per target):

- **Site-operations map**: `keep requests that reveal how this site operates:
  API calls, config/feature-flag endpoints, analytics/telemetry beacons,
  A/B testing, data collection. skip static assets, ad-tech, google requests.`
- **First-party API surface**: `keep first-party API calls (fetch/xhr, /api/
  style paths) | "\(.request.method) \(.request.url) -> \(.response.status)"`
- **Feature flags/experiments** (highest signal): filter, then project
  `{url, body: .response.content.text}`.
- **Third-party data exfil**: `keep requests sending user behavior data
  (clicks, pageviews, fingerprints) to third parties`, project
  `{url, sample: (.request.postData.text // .response.content.text // "" | .[0:300])}`
- **Errors**: `keep requests with 4xx/5xx or aborted` → status+method+url.
- **PII exposure**: `keep requests containing emails/names/IDs/location sent
  to third parties`.

HAR gotchas: session-replay vendors (Quantummetric, Hotjar, FullStory) record
everything and are usually the top finding; beacons with 204 responses carry
their payload in `.request.postData.text`, not the response; count survivors
with `| wc -l` to sanity-check fail-open drops.

## Guardrails

- Only run recon against sites you're **authorized** to test, or for passive
  public-surface analysis. Never probe, fuzz, or authenticate — this is
  capture-and-read-only analysis of requests the site itself serves.
- Don't exfiltrate: if a filter surfaces credentials/tokens in bodies, report
  their existence and path, don't copy values into outputs or commits.
- Large filters cost API tokens; prefer the local llama provider for bulk
  exploratory runs, remote providers for quality-critical passes.

## Provenance

Skill created 2026-09-14 alongside jqlm's development (fork of gojq v0.12.19; jev builtins added 2026-10-07)
after live HAR recon and bulk-filtering sessions proved the workflow. Helper
script `scripts/har_capture.py` was battle-tested on southwest.com.
