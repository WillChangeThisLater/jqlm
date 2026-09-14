---
name: jqlm
description: Use jqlm (a jq fork with llm_select/llm_judge builtins) for semantic filtering of JSON streams — especially HAR network-capture recon of websites (authorized pen-testing, understanding how a site operates), log triage, and large-scale "which of these items matter" questions.
---

# jqlm — semantic filtering for JSON (HAR recon and friends)

`jqlm` is a fork of gojq (bug-for-bug jq in Go) with two LLM builtins. Repo:
`~/repos/jqlm` (github: WillChangeThisLater/jqlm). Binary on PATH via
`~/.local/bin/jqlm`. Read `~/repos/jqlm/README.md` and `PROOF.md` for details.

## The builtins

```jq
llm_select(value; "prompt")   # -> bool: true = keep. Use inside select().
llm_judge(value; "prompt")    # -> {keep: bool, reason: string}
```

One HTTP request per item to the configured provider. **Failed calls fail
open: the item is DISCARDED with a stderr warning.** Always glance at stderr
for `N of M llm call(s) failed` — an outage silently shrinks output otherwise
(by design; the warning is the tell).

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

## Critical usage rules

1. **Parallelism is per input value.** A single big JSON array is ONE input =
   sequential. Pre-split: `jq -c '.[]' big.json | jqlm --llm-concurrency N
   '...'`. Output order is preserved; `jq -s` to rewrap.
2. Big single documents (e.g. an entire 3MB HAR as one value) are handled
   fine but filtered sequentially — slice before filtering when speed matters.
3. Providers may **silently truncate** oversized items (OpenRouter does).
   Set `JQLM_MAX_ITEM_BYTES` (env or config) to discard explicitly with a
   warning instead of trusting a truncated verdict.
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

Skill created 2026-09-14 alongside jqlm's development (fork of gojq v0.12.19)
after live HAR recon and bulk-filtering sessions proved the workflow. Helper
script `scripts/har_capture.py` was battle-tested on southwest.com.
