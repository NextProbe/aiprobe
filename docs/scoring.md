# Context Efficiency Score

*[中文版](scoring.zh-CN.md)*

The Context Efficiency Score is a number from 0 to 100 that answers one question:
**how much of what this session paid for was actually useful context?**

Two rules govern it:

1. **It is deterministic.** The score is a closed-form function of provider token
   counters and AIProbe's own analyzers. Running the same session twice gives
   the same number, on any machine, forever.
2. **No model is involved.** Not in the sub-scores, not in the weights, not in
   the letter grade. A language model never sees this session and never scores
   it. This document is the whole definition.

The implementation lives in [`internal/analyzer/score/score.go`](../internal/analyzer/score/score.go).
A test (`TestDocMatchesImplementation`) fails the build if this document and the
code disagree about a component key, a weight or a formula.

---

## Components and weights

| Key | Component | Weight |
| --- | --- | --- |
| `cache_efficiency` | Cache efficiency | 30 |
| `context_uniqueness` | Context uniqueness | 25 |
| `mcp_utilization` | MCP tool utilization | 15 |
| `tool_output_efficiency` | Tool output efficiency | 15 |
| `prefix_stability` | Prefix stability | 15 |

Nominal weights sum to 100.

`clamp01(x)` below means `min(1, max(0, x))`.

---

## 1. Cache efficiency — weight 30

How much of the prompt the provider served from cache instead of billing at full
input price.

```
raw   = cache_read_tokens / prompt_tokens
score = clamp01( cache_read_tokens / prompt_tokens / 0.85 )
```

- `cache_read_tokens` and `prompt_tokens` are **measured**, summed over every
  request from `cache_read_input_tokens` and
  `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`.
- The target is **0.85**. A long agent session that reuses its prefix properly
  sits above 85% cache reads, because the conversation grows monotonically while
  only the newest turn is fresh. Hitting the target scores full marks; there is
  no reward for exceeding it, since the remainder is the work itself.

Unknown when the session records no prompt tokens, or when it contains fewer than
two requests. A first request has no cached prefix to reuse, so its hit ratio is
structurally zero and carries no information about efficiency.

## 2. Context uniqueness — weight 25

How much of the tool output the agent pulled into context was new information
rather than a repeat of something already there.

```
raw   = 1 - duplicate_insert_waste / tool_output_tokens
score = clamp01( ((1 - duplicate_insert_waste / tool_output_tokens) - 0.60) / 0.40 )
```

- `duplicate_insert_waste` is the estimated token cost of redundant copies found
  by the duplicate detector: repeated file reads, repeated shell output, and
  exact or near-identical tool results. It counts the copies, never the first
  occurrence.
- `tool_output_tokens` is the estimated cost of all tool output in the session.
- The floor is **0.60**. Below 60% unique tool output the session is mostly
  paying to re-read things it already had, and scores zero on this component;
  100% unique scores full marks.

Unknown when the session produced no tool output.

## 3. MCP tool utilization — weight 15

How much of the loaded MCP tool surface the session actually used. Tool
definitions are re-sent on every request whether they are called or not.

```
raw   = tools_used / tools_loaded
score = clamp01( (tools_used / tools_loaded) / 0.50 )
```

- `tools_used` is the number of distinct MCP tools called at least once.
- `tools_loaded` comes from a tool inventory supplied with `--tools`.
- The target is **0.50**. Half the loaded tools being useful is treated as
  healthy; nobody calls every tool in every session, and demanding more would
  punish reasonable setups.

Unknown when:

- no MCP server was involved in the session, or
- no tool inventory is available, so `tools_loaded` is unknown. AIProbe will
  not guess a denominator.

## 4. Tool output efficiency — weight 15

Whether tool results arrive in usable sizes or as bulk dumps. One 40k-token file
read dominates a context for the rest of the session.

```
raw   = 1 - oversized_output_tokens / tool_output_tokens
score = clamp01( ((1 - oversized_output_tokens / tool_output_tokens) - 0.30) / 0.70 )
```

- `oversized_output_tokens` is the estimated cost of tool results at or above
  **4000 tokens** each. The threshold is configurable with
  `--oversized-result-tokens`; the score always uses the value in effect for the
  run and reports it.
- The floor is **0.30**. A session where more than 70% of tool output came from
  oversized single results scores zero here.

Unknown when the session produced no tool output.

## 5. Prefix stability — weight 15

Whether the front of the prompt stayed put. Anything that changes the prefix
invalidates every cache entry after it.

```
raw   = 1 - avoidable_cache_miss_tokens / prompt_tokens
score = clamp01( ((1 - avoidable_cache_miss_tokens / prompt_tokens) - 0.70) / 0.30 )
```

- `avoidable_cache_miss_tokens` is **measured**: for each request, the previous
  request's prompt size minus this request's `cache_read_input_tokens`, counted
  when the shortfall exceeds `max(1024, 3%)` of the previous prompt.
- Drift attributed to **context compaction is excluded**. Compaction is the agent
  working as designed, not a fixable configuration mistake, so it does not cost
  points.
- The floor is **0.70**. Losing more than 30% of all prompt tokens to avoidable
  cache misses scores zero.

Unknown when the session has fewer than two requests, or records no prompt
tokens.

---

## Combining the components

Components that cannot be evaluated are reported as **unknown**, and their weight
is redistributed proportionally across the components that could be evaluated:

```
known_weight       = sum of nominal weights of evaluable components
effective_weight_i = weight_i / known_weight * 100
points_i           = score_i * effective_weight_i
value              = round( sum of points_i )
```

Because effective weights always sum to 100, the points sum straight to the final
0–100 value.

A session is therefore never penalised for data AIProbe could not observe. A
project with no MCP servers is not marked down for having no MCP utilization; the
remaining components simply carry more weight. The report always states which
components were redistributed and why.

If nothing at all can be evaluated the score is reported as `n/a` rather than 0.

## Grades

| Value | Grade |
| --- | --- |
| 85–100 | A |
| 70–84 | B |
| 55–69 | C |
| 40–54 | D |
| 0–39 | F |

Grades are a reading aid. The component table is the useful part, because it
points at which change is worth making.

---

## Measured vs estimated

The score mixes measured and estimated inputs, and the distinction matters when
you act on it:

| Input | Provenance |
| --- | --- |
| `prompt_tokens`, `cache_read_tokens` | Measured. Provider usage counters. |
| `avoidable_cache_miss_tokens` | Measured. Derived from provider counters only. |
| `tools_used`, `tools_loaded` | Exact counts, from the transcript and the supplied inventory. |
| `duplicate_insert_waste`, `tool_output_tokens`, `oversized_output_tokens` | Estimated. Token counts come from AIProbe's offline estimator. |

The estimator is a deterministic character-class heuristic documented in
[`internal/tokenize/tokenize.go`](../internal/tokenize/tokenize.go). It carries
roughly ±15% error on mixed agent traffic, and where a session provides
consecutive requests, it is calibrated against measured prompt growth to reduce
that. It never rewrites a measured total: provider numbers win, and estimated
parts are never rescaled to make a total look exact.

Practically: components 1 and 5 are as accurate as the provider's own billing.
Components 2, 3 and 4 rank problems reliably but their absolute token figures are
estimates, which is why every such number in a report carries a leading `~`.

## Why these five

Each component maps to a change someone can actually make:

| Component | The change it points at |
| --- | --- |
| Cache efficiency | Stop invalidating the prefix; keep sessions warm. |
| Context uniqueness | Stop re-reading the same files and re-running the same commands. |
| MCP tool utilization | Load fewer tools, or load them lazily. |
| Tool output efficiency | Cap output at the source; read ranges, not whole files. |
| Prefix stability | Keep rules, tool config and model fixed for the length of a session. |

A metric that does not lead to one of those changes is not in the score.
