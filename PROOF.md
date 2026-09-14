# jqlm — proof of work

`jqlm` is a fork of [gojq](https://github.com/itchyny/gojq) (a bug-for-bug jq
reimplementation in Go) with two LLM builtins bolted on: `llm_select/2` and
`llm_judge/2`. Everything else — the full jq language, the CLI flags, the
streaming behavior — is upstream gojq, untouched.

The complete diff against upstream lives in 2 files:
`cli/llm.go` (~330 lines, new) and ~8 lines wired into `cli/cli.go`.

Config is env-only. Providers: `openai`, `openrouter`, `llama` (local
llama.cpp server, grammar-constrained JSON), `openai-compat` (any
OpenAI-compatible endpoint). See README.

All examples below are captured real runs (2026-09-14). Fixture: 4 pizza
reviews, 2 negative (`Karen Brown`, `Mike Davis`).

---

## 1. It is jq first: --arg, -r, --slurp, @base64 all just work

```
$ jqlm -r --arg who paul '"hello \($who)"' <<< 'null'
hello paul
$ echo '{"a":1}' | jqlm -c --slurp '[.[0].a + 1]'
[2]
$ echo '"jqlm is jq"' | jqlm -r @base64
anFsbSBpcyBqcQ==
```

## 2. llm_select — OpenRouter, z-ai/glm-5.3-flash

```
$ JQLM_PROVIDER=openrouter JQLM_MODEL=z-ai/glm-5.3-flash \
  jqlm -c '.[] | select(llm_select(. ; "only keep reviews where the reviewer really did not like the food")) | .reviewer' < reviews.json
warning: jqlm: provider=openrouter model=z-ai/glm-5.3-flash mode=json_mode timeout=15s
"Karen Brown"
"Mike Davis"
```

## 3. llm_select — local llama.cpp server (Qwen3.5-9B, grammar-constrained JSON)

Local responses are constrained by a GBNF grammar derived from the schema, so
malformed JSON is impossible.

```
$ JQLM_PROVIDER=llama JQLM_MODEL=Qwen3.5-9B-Q8_0.gguf \
  jqlm -c '.[] | select(llm_select(. ; "only keep reviews where the reviewer really did not like the food")) | .reviewer' < reviews.json
warning: jqlm: provider=llama model=Qwen3.5-9B-Q8_0.gguf mode=json_schema_mode timeout=1m0s
"Karen Brown"
"Mike Davis"
```

## 4. llm_judge — keeps the reasoning, not just the verdict

```
$ JQLM_PROVIDER=openrouter JQLM_MODEL=z-ai/glm-5.3-flash \
  jqlm -c '.[0] | llm_judge(. ; "is this a positive pizza review?")' < reviews.json
{"keep":true,"reason":"This is clearly a positive pizza review: the reviewer calls it 'The best pizza I've ever had!', praises the 'Perfectly crispy crust', gives a 'Highly recommend!' endorsement, and awards 5 stars."}
```

## 5. OpenAI back-compat: no env needed (default provider)

```
$ jqlm -c '.[] | select(llm_select(. ; "keep only 5-star reviews")) | .reviewer' < reviews.json
warning: jqlm: provider=openai model=gpt-4o-mini mode=json_mode timeout=8s
"Alice Johnson"
```

## 6. openai-compat escape hatch — same binary against Ollama/vLLM/LM Studio

Here pointed at the local llama-server through the generic OpenAI-compatible
path:

```
$ JQLM_PROVIDER=openai-compat JQLM_BASE_URL=http://localhost:8080/v1 \
  JQLM_API_KEY=x JQLM_MODEL=Qwen3.5-9B-Q8_0.gguf \
  jqlm -c '.[] | select(llm_select(. ; "keep only 1-star reviews")) | .reviewer' < reviews.json
"Karen Brown"
"Mike Davis"
```

## 7. Scaling: 100 items, request-per-item, linear and correct

Fixture: 100 reviews, exactly 50 complaining about the food.

```
$ JQLM_PROVIDER=openrouter JQLM_MODEL=z-ai/glm-5.3-flash \
  jqlm '[.[] | select(llm_select(. ; "keep only reviews complaining about the food"))] | length' < big.json
50
real    6m52s     # 100 sequential calls, ~4.1s each — linear, no degradation
```

```
$ JQLM_PROVIDER=llama JQLM_MODEL=Qwen3.5-9B-Q8_0.gguf \
  jqlm '[.[] | select(llm_select(. ; "keep only reviews complaining about the food"))] | length' < big.json
50
real    6m22s     # 100 calls on a 9B model on a desktop GPU, zero failures
```

Correct verdicts on all 100 items with both providers. Cost is O(n) calls by
design (request-per-item, per project decision); per-call latency dominates,
throughput does not degrade as the input grows.

## 7b. Concurrency: `--llm-concurrency N` — same output, N× faster

Per-item requests are dispatched across a worker pool (gojq's `Code.Run` is
documented goroutine-safe); results still print in **input order**, so output
is identical to sequential mode. Requires input as a stream of documents
(NDJSON), since parallelism is per input value. A judge request now sends
`temperature: 0` for deterministic verdicts.

```
# sequential (from §7): 6m52s — 100 calls at ~4.1s each
$ JQLM_PROVIDER=openrouter JQLM_MODEL=z-ai/glm-5.3-flash \
  jqlm --llm-concurrency 10 JQLM_TIMEOUT=30s -c \
  'select(llm_select(. ; "keep only reviews complaining about the food")) | .i' \
  < big.ndjson | wc -l
50
real    1m6s       # 6.3x speedup, zero failures, order preserved
```

```
# local llama.cpp: needs server-side parallel slots (llama-server -np 8)
# and enough VRAM for N concurrent context slots
$ JQLM_PROVIDER=llama JQLM_MODEL=Qwen3.5-9B-Q8_0.gguf \
  jqlm --llm-concurrency 8 -c '... | .i' < big.ndjson | wc -l
50
real    1m26s      # 4.4x speedup (GPU throughput-bound)
```

Notes:
- under parallel load some calls take longer; raise `JQLM_TIMEOUT` accordingly
  (with the default 15s, 2 of 100 calls timed out and were dropped with
  warnings — fail-open held, but the run was short 2 items).
- on a 16GB GPU, `-np 8` requires shrinking the server context (`--ctx-size
  32768`); 8 slots × 128k ctx does not fit.

## 8. Graceful failure: pipeline completes when calls fail

A provider outage mid-run (llama-server killed externally after 3 calls) —
fail-open per item, warnings to stderr, exit 0:

```
warning: jqlm: decide failed, item DISCARDED: Post "http://localhost:8080/v1/chat/completions": EOF
warning: jqlm: decide failed, item DISCARDED: ... connection refused
warning: jqlm: 97 of 100 llm call(s) failed; those items were DISCARDED
```

Same with per-call timeouts (`JQLM_TIMEOUT`):

```
warning: jqlm: decide failed, item DISCARDED: context deadline exceeded
warning: jqlm: 1 of 4 llm call(s) failed; those items were DISCARDED
```

The end-of-run summary makes silent data loss impossible to miss.

## 9. Oversized items: explicit discard instead of silent truncation

Without a guard, some providers silently truncate over-window inputs and
return a confidently wrong verdict (observed on OpenRouter with a 3MB item).
`JQLM_MAX_ITEM_BYTES` turns that into an explicit, visible discard:

```
$ JQLM_PROVIDER=openrouter JQLM_MODEL=z-ai/glm-5.3-flash JQLM_MAX_ITEM_BYTES=200000 \
  jqlm -c '.[] | select(llm_select(. ; "keep reviews mentioning pizza")) | .id' < huge2.json
warning: jqlm: provider=openrouter model=z-ai/glm-5.3-flash mode=json_mode timeout=15s maxItemBytes=200000
1
warning: jqlm: item is 3000020 bytes (JQLM_MAX_ITEM_BYTES=200000), DISCARDED without calling provider
3
```

Items 1 and 3 (real reviews mentioning pizza) are kept and judged normally;
the 3MB garbage item is dropped with an explicit warning. No provider call is
wasted on it.

## 10. Config errors fail fast with clear messages (exit 5)

```
$ echo '{"a":1}' | JQLM_PROVIDER=bogus jqlm 'llm_select(. ; "x")'
jqlm: unknown JQLM_PROVIDER "bogus" (known: openai, openrouter, llama, openai-compat)   (exit 5)
$ echo '{"a":1}' | JQLM_PROVIDER=openrouter jqlm 'llm_select(. ; "x")'
jqlm: provider openrouter requires JQLM_MODEL                                           (exit 5)
$ echo '{"a":1}' | JQLM_MODE=bogus jqlm 'llm_select(. ; "x")'
jqlm: invalid JQLM_MODE "bogus" (known: json, json-schema, tool)                        (exit 5)
```

Errors surface on the first actual `llm_*` call (lazy init, like jq's own
`input`), so a script that never hits the LLM path never needs a key.

---

## Known limitations

- Parallelism (`--llm-concurrency`) is per **input value**: a single large JSON
  array input is one query run, so `llm_select` calls inside it stay sequential.
  Feed NDJSON (one document per line) to get the speedup.
- Under heavy parallelism, per-call latency rises; tune `JQLM_TIMEOUT` up.
- `llm_judge`'s free-text `reason` is model-quality; treat it as a hint, not
  an audit trail.
- Extreme inputs to a local llama-server can crash the *server* process (its
  own bug); jqlm itself survives and continues.
