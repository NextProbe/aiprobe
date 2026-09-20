# The token reduction playbook

*[中文版](playbook.zh-CN.md)*

AIProbe tells you where an agent session's tokens went. This document is the other
half: what people have built to get them back, what each approach actually saves,
and how to tell whether it worked on your workload rather than on someone's
benchmark.

Findings in a report link here. If you arrived from a `docs/playbook.md#something`
reference, that section is the one you want.

## Read this first

**Measure, change one thing, measure again.** Every technique below has a number
attached somewhere on the internet. Almost none of those numbers will be your
number, because they were measured on a different workload. The only figure worth
acting on is the one from your own sessions, which is what AIProbe exists to
produce:

```sh
aiprobe analyze                     # before
# change exactly one thing
aiprobe analyze                     # after
aiprobe history                     # the trend across both
```

**"Saving tokens" is four unrelated problems.** They get discussed as one topic and
they are not. Confusing them is how people install a KV-cache tool to reduce an
Anthropic bill.

| | What it reduces | Where it runs |
| --- | --- | --- |
| **Context compression** | tokens you send | before the request |
| **Caching** | requests you make at all | before the request |
| **Model routing** | price per token, not token count | at dispatch |
| **KV cache compression** | GPU memory during inference | inside the model server |

If you call a hosted API, the last one is irrelevant to you no matter how
impressive its numbers are.

**The multiplier nobody mentions.** In an agent session, a token you add is not
paid once. It is paid by every subsequent request that carries it. A 5,000-token
file read in turn 3 of a 40-turn session costs about 185,000 tokens of exposure,
not 5,000. This is why AIProbe reports a "billed" column separately from an
"inserted" column, and why removing a little content early is worth more than
removing a lot late.

## The six layers

```
                        agent / IDE / CLI
                               │
   ① exact cache ──────────────┤   have I answered this exact thing before?
   ② semantic cache ───────────┤   have I answered something close enough?
                               │
   ③ tool output compression ──┤   the output is 5,000 lines; 20 matter
   ④ prompt compression ───────┤   the always-on preamble is too long
   ⑤ history compaction ───────┤   the conversation is now the cost
                               │
   ⑥ model routing ────────────┤   this turn does not need the big model
                               ▼
                            provider
```

Layers ① and ② avoid the call. Layers ③ to ⑤ make the call smaller. Layer ⑥ makes
it cheaper. They compose, and they are worth doing roughly in that order, because
avoiding a call beats shrinking it.

### Which layer does your report point at?

| AIProbe finding | Layer | Section |
| --- | --- | --- |
| R008 duplicate tool results, R009 repeated commands, R010 oversized output, R016 tool hotspot | ③ | [tool output compression](#tool-output-compression) |
| R003 repeated file reads, R011 full-file reads | ③ | [locate before you read](#locate-before-you-read) |
| R012 fixed per-request overhead | ④ | [prompt compression](#prompt-compression) |
| R013 compaction thrash, R017 conversation dominates | ⑤ | [history compaction](#history-compaction) |
| R001 / R002 MCP tool tax | — | this is configuration, not compression: turn servers off |
| R004 to R007, R014 prompt cache | — | cache *hygiene*, covered under [keeping the prefix stable](#keeping-the-prefix-stable) |

## Tool output compression

*Layer ③.* The highest-yield layer for a coding agent, because tool output is usually the
largest category in the report and the easiest to cut without losing anything.

The insight worth internalising, which the RTK authors state plainly: **if a
deterministic rule can do it, do not use a model.** A test run with 999 passes and
one failure does not need a language model to decide what matters.

### jq

[jq](https://github.com/jqlang/jq) — the one you almost certainly already have.

The cheapest compression is not fetching the rest. A tool that returns fifty fields
when the next decision needs two is not a compression problem, it is a query
problem:

```sh
gh pr view 42 --json title,state,files | jq '{title, state, files: [.files[].path]}'
kubectl get pods -o json | jq '.items[] | select(.status.phase != "Running") | .metadata.name'
```

**Why it belongs at the top of this layer.** Every other tool here decides what to
throw away *after* the data exists. Selecting fields at the source cannot lose
something the model needed, because you chose what it needed. There is no model, no
ratio to tune and no quality risk.

**How to measure it.** The Tool Result row. A large drop with no change in outcome is
the clearest win available in this document.

### rtk

[github.com/rtk-ai/rtk](https://github.com/rtk-ai/rtk) — MIT, single Rust binary,
no network, no telemetry.

A CLI proxy that sits between the agent's shell command and the output the model
sees. It has per-command handlers for around a hundred common developer commands
(git, test runners, build tools, package managers, container tooling) and applies
noise filtering, grouping, deduplication and truncation — keeping failures,
dropping the parts that never affect a decision.

```
go test ./...        →   126 tests passed
5,000 lines              FAIL: TestXYZ  xxx.go:137  expected 10, got 11
```

**When it applies.** Your report shows a large Bash Output row, or R009 / R016
fired.

**How to measure it.** Run your usual session, then `aiprobe analyze` and compare
the Bash Output row and the tools table.

**What its numbers mean.** The project reports 60–90% reduction with under 10ms
overhead. Read that carefully: it is a share of *bash output*, and the project says
so explicitly — bash output is one category of your context, not your bill. If bash
output is 20% of your prompt, a 90% cut there is an 18% cut overall.

**When not to use it.** When you need the full output for something other than the
agent's next decision — a CI artifact, a log you are grepping yourself.

### headroom

[github.com/headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom)
· [docs.headroomlabs.ai](https://docs.headroomlabs.ai/)

A context optimization layer that sits between the application and the provider. Two
ideas are worth taking whether or not you adopt the tool:

**Content-aware compression.** JSON, code, logs, diffs, HTML and prose compress
well by different rules. A generic truncation treats them all as text and throws
away the wrong half. Compressing a JSON array by keeping the schema and a sample of
rows preserves what the model needs; cutting it at 2,000 characters does not.

**CCR — compress, cache, retrieve.** Send a compressed form, keep the original in a
local store keyed by hash, and give the model a tool to fetch the original back if
it turns out to need it. This is the move that makes aggressive compression safe:
the information is not gone, it is one call away. Content addressing by hash also
means the compressed form is stable, which matters for prompt caching (see
[keeping the prefix stable](#keeping-the-prefix-stable)).

**When it applies.** Large Tool Result or File Read rows, R008 or R010 fired, or
you are building a gateway of your own and want the architecture.

**How to measure it.** Compare the Tool Result and File Read rows before and after
— *and check the answers are still correct*. Compression that changes conclusions is
not a saving, and this is the failure mode to watch for with any lossy layer.

**What its numbers mean.** The project reports 60–95% fewer tokens, with
per-workload figures of 92% on SRE incident debugging, 73% on GitHub issue triage
and 92% on code search. Those are the project's own measurements on its own
workloads. Treat them as evidence the approach works, not as a forecast for yours.

### Capsule

Agent-context compression aimed specifically at Codex, MCP and similar agent
runtimes, covering terminal output, files, project context and web pages, with
exact-recoverable compression in the same spirit as CCR.

Newer and less proven than the two above. Worth tracking if your waste is
concentrated in agent-generated context rather than in shell output.

**When it applies.** R016 fired — a tool that dominates by accumulation rather than
by any single large result.

### locate before you read

Not a project, a habit, and the highest-yield change most people can make in an
afternoon.

Reading a whole file to find one function is the most common avoidable waste in
agent sessions, and it is expensive twice: once for the tokens, and again because
the file stays in context for every later request.

```sh
rg -n 'func Login' internal/          # find the line numbers
# then read a window: offset ≈ hit - 40, limit ≈ 120
```

- [ripgrep](https://github.com/BurntSushi/ripgrep) — fast enough that locating
  first becomes the natural habit rather than an extra step.
- [ast-grep](https://github.com/ast-grep/ast-grep) — searches by syntax rather than
  text, so you can find every call site of a function without opening the files
  that contain them.

Then write the habit into your agent's rules file so you are not relying on
remembering it:

```
Locate with Grep before reading. Read ranges (offset/limit), not whole files.
Do not re-read a file to refresh memory; it is already in the conversation.
```

**How to measure it.** The File Read row, and the full-file read count in R011.

## Prompt compression

*Layer ④.* This is about the material that is on every single request: the system
prompt, your rules file, tool schemas. It is smaller than tool output but it is
paid every turn, which is why R012 exists.

Attack it in this order, because the cheap options are also the safe ones:

1. **Delete.** Most rules files contain paragraphs that never changed a model's
   behaviour. Cut to what does.
2. **Move.** Reference material belongs in a file the agent reads on demand, not in
   the preamble.
3. **Unload.** Every loaded tool's schema is re-sent every turn. This is the MCP
   tool tax, and turning a server off is a bigger, safer win than compressing text.
4. **Only then compress**, with the tools below.

### LLMLingua

[github.com/microsoft/LLMLingua](https://github.com/microsoft/LLMLingua) —
Microsoft. Three related pieces:

- **LLMLingua** — a small model scores token importance and drops the low-value
  ones. The output reads oddly to a human and is usually still understood by the
  model.
- **LongLLMLingua** — *query-aware*: compresses retrieved documents relative to the
  question being asked, so it ranks and prunes rather than uniformly shrinking. This
  is the one that matters for RAG.
- **LLMLingua-2** — reframed as token classification; reported as several times
  faster than the first generation and generally the one to start with.

**Published claims, and what to expect.** Microsoft reports up to ~20x compression
for LLMLingua and roughly quarter-token usage on long-context and RAG tasks for
LongLLMLingua. In production, aiming for 100k → 50–70k is a sane target; chasing
20x on prose you actually depend on is how you lose information you needed.

**When it applies.** R012 fired and your fixed per-request overhead is dominated by
prose you cannot delete.

**When not to use it.** On code, structured output, or anything where exact tokens
matter. Dropping "low-information" tokens from a JSON payload or a diff corrupts it.

**How to measure it.** Compress the rules file, re-analyse, compare the fixed
overhead per request figure. Treat 2x as a good result.

### Selective Context

[github.com/liyucheng09/Selective_Context](https://github.com/liyucheng09/Selective_Context)

Same family, different criterion: it drops lexical units with low
*self-information* — the more predictable a word is, the less it carries. The
project's framing is roughly 2x more content in the same window.

Academically interesting; for production prompt compression today, LLMLingua-2 is
the more practical starting point.

### PCToolkit

[github.com/3DAgentWorld/Toolkit-for-Prompt-Compression](https://github.com/3DAgentWorld/Toolkit-for-Prompt-Compression)

Not a compressor — a harness that puts LLMLingua, LLMLingua-2, Selective Context,
SCRL and KiS behind one interface with common metrics.

This is the right first stop if you are considering building a compression layer.
Do not pick a method from published benchmarks; run PCToolkit over *your* prompts
and compare compression ratio against quality loss on your own tasks. The answer is
frequently "the rule-based approach was good enough", which is a valuable result to
reach before writing an ML pipeline.

## History compaction

*Layer ⑤.* Eventually the conversation itself is the largest line in the report.
R017 fires on exactly this, and no amount of tool output compression addresses it,
because the cost is the accumulated trajectory.

The pattern, and the reason it beats per-message compression: after a stretch of
work, replace the trajectory with a written summary — what was found, what remains
open, what the current plan is — and continue from that. Compressing 300
observations by 20% each still leaves you carrying 300 observations.

**ReSum** (Alibaba) is the worked version of this for long-horizon agents, aimed at
agents that run for hundreds of steps and would otherwise be dragged down by their
own history.

You do not need a framework to apply it:

- End a session at a task boundary rather than letting one session run all day.
- Before the runtime compacts for you, write the handover yourself into a file the
  next session reads. Then the summary is yours, and it keeps what you care about.
- One session per task makes the prompt cache work better too, because the prefix
  stops churning.

**How to measure it.** The Messages row, and the conversation-at-session-end figure,
in the *next* session.

## Keeping the prefix stable

Not compression at all — the cheapest layer, and the one most often broken by
accident. Providers charge a fraction of the input price for a cache hit, and a hit
requires the front of the prompt to be byte-identical to last time.

Things that throw the cache away, all of which AIProbe attributes explicitly in its
cache causes table:

- editing your rules file mid-session (R007)
- switching models mid-session — cache entries are per-model (R006)
- idle gaps longer than the cache TTL (R005)
- timestamps, run ids or "current state" blocks in always-on context (R007)
- context compaction, which invalidates the prefix by design (R013)

The fix is behavioural and free: put volatile material at the *end* of the prompt,
keep the front stable, and work in continuous stretches. Compression that makes the
prefix unstable can cost more than it saves, which is why content-addressed
compression (CCR above) is a better fit than anything that re-renders differently
each turn.

## Caching

*Layers ① and ②.* For a single developer using a coding agent, these matter less — your questions are
rarely repeats. For a team behind a gateway they can beat every other layer,
because a cache hit costs nothing.

- **[GPTCache](https://github.com/zilliztech/GPTCache)** — the well-known
  open-source semantic cache. Embeds the request, and if something close enough was
  answered before, returns that.
- **[LiteLLM](https://github.com/BerriAI/litellm)** — a gateway with routing, cost
  tracking, fallbacks, rate limiting and both exact and semantic caching (Redis
  backed). If you are building a gateway rather than researching compression, start
  here; you get the rest of the plumbing with it.

**The risk to hold in mind.** A semantic cache answers a question that is merely
similar. For "how do I request a test environment" that is fine. For anything where
the difference between two similar questions is the point, it is a correctness bug
with a cost saving attached. Set the similarity threshold conservatively and log
what it serves.

## Model routing

*Layer ⑥.* Route easy turns to a small model and hard turns to a large one. Note what this
does and does not do: it lowers price per token, it does not reduce tokens. Both
save money; only one shows up in a token report.

[vLLM Semantic Router](https://github.com/vllm-project/semantic-router) is the
open-source implementation to look at. Be aware of the interaction with layer ①:
switching models invalidates the prompt cache, so routing inside a single
conversation can cost more than it saves. Route between sessions, not within one.

## Self-hosted inference

**[NVIDIA KVPress](https://github.com/NVIDIA/kvpress)** compresses the KV cache
*inside* the inference server — the memory holding attention keys and values for
tokens already processed. NVIDIA's illustration of the scale: a 70B model at 1M
tokens of context can need hundreds of gigabytes of KV cache in FP16. KVPress
implements SnapKV, StreamingLLM, PyramidKV, TOVA, KVzip and others to decide what
to evict.

Genuinely important if you run vLLM or serve your own Llama or Qwen. Completely
irrelevant if you call a hosted API: you are billed for tokens, not for the
provider's GPU memory.

## If you only do three things

1. **Stop reading whole files.** Locate with `rg`, read a range. Write it into your
   rules file. Costs nothing, and it is usually the largest single row in the report.
2. **Put a deterministic filter in front of your shell output.** rtk, or just
   `| tail -20` and `--quiet` flags. No model required, no quality risk.
3. **End sessions at task boundaries.** A session that has run for hundreds of
   requests pays for its whole history on every one of them.

Then re-run `aiprobe analyze` and see which layer your remaining waste is in.

## A note on the numbers in this document

Every percentage attributed to a project is that project's own published claim,
measured on its own workloads, and is labelled as such above. None of them has been
independently reproduced here.

AIProbe does not install, call, bundle or endorse any of these tools. It names them
because they address patterns it can measure, and every reference above comes with
the measurement you can make yourself afterwards. If a tool does nothing for your
workload, the report will show that, and that is the point.

## Sources

- rtk — [github.com/rtk-ai/rtk](https://github.com/rtk-ai/rtk)
- Headroom — [github.com/headroomlabs-ai/headroom](https://github.com/headroomlabs-ai/headroom), [docs.headroomlabs.ai](https://docs.headroomlabs.ai/)
- LLMLingua / LongLLMLingua / LLMLingua-2 — [github.com/microsoft/LLMLingua](https://github.com/microsoft/LLMLingua)
- Selective Context — [github.com/liyucheng09/Selective_Context](https://github.com/liyucheng09/Selective_Context)
- PCToolkit — [github.com/3DAgentWorld/Toolkit-for-Prompt-Compression](https://github.com/3DAgentWorld/Toolkit-for-Prompt-Compression)
- GPTCache — [github.com/zilliztech/GPTCache](https://github.com/zilliztech/GPTCache)
- LiteLLM — [github.com/BerriAI/litellm](https://github.com/BerriAI/litellm)
- vLLM Semantic Router — [github.com/vllm-project/semantic-router](https://github.com/vllm-project/semantic-router)
- NVIDIA KVPress — [github.com/NVIDIA/kvpress](https://github.com/NVIDIA/kvpress)
- jq — [github.com/jqlang/jq](https://github.com/jqlang/jq)
- ripgrep — [github.com/BurntSushi/ripgrep](https://github.com/BurntSushi/ripgrep)
- ast-grep — [github.com/ast-grep/ast-grep](https://github.com/ast-grep/ast-grep)

Descriptions above are paraphrased and condensed from each project's own
documentation. Content was rephrased for compliance with licensing restrictions.
