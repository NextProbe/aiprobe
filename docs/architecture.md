# Architecture

*[中文版](architecture.zh-CN.md)*

How AIProbe gets from a session file on disk to a report, and which design
decisions are load-bearing.

For what writing an adapter involves, see
[CONTRIBUTING.md](../CONTRIBUTING.md#adding-an-adapter). For the scoring formulas,
[scoring.md](scoring.md).

## The pipeline

```
session file
     │
     ▼
adapters.Detect ──▶ adapters.Parse ──────▶ *trace.Trace
   (claude | kiro | kiro-cli | cursor | codex)  │
                                            ▼
                              ┌─── analyzer.Analyze ───┐
                              │                        │
                    context.Compute            mcp.Analyze
                    duplicate.Analyze          cache.Analyze
                              │                        │
                              └──▶ rules.Evaluate ─────┘
                                   score.Compute
                                   pricing (costs)
                                            │
                                            ▼
                                     analyzer.Result
                                            │
                     ┌──────────────────────┼──────────────────────┐
                     ▼                      ▼                      ▼
              report/terminal          report/html            report/json
                                                                   │
                                                                   ▼
                                                              store (history)
```

The store sits on the JSON renderer rather than beside it: what it writes to disk
*is* the JSON report, so re-reading a stored analysis gives back the same
`analyzer.Result` any other consumer would get. See
[Persisting analyses](#persisting-analyses).

For more than one session the same pipeline runs per session, and the *results*
are folded together:

```
[]analyzer.Result ──▶ overview.Build ──▶ *overview.Report
                                              │
                            ┌─────────────────┼─────────────────┐
                            ▼                 ▼                 ▼
                  terminal.WriteOverview  html.RenderOverview  JSON
```

Data flows one way. An analyzer reads the trace and writes its own result
struct; it never mutates the trace and never calls another analyzer. Where one
analyzer's output feeds another — the MCP schema estimate refines the context
breakdown's residual — it is passed explicitly as an argument
(`context.Compute(tr, split)`), not discovered through a shared mutable object.

## The trace model

`internal/trace` is the boundary between "how a runtime happens to write files"
and "what AIProbe knows". Everything above it is runtime-agnostic; a package
name like `claude` or `kiro` appears nowhere in an analyzer.

A `Trace` is four parallel views of one session:

| Field | What it is |
| --- | --- |
| `Events` | The normalized timeline. Every message, tool call, tool result and system record, in order, each with an estimated token cost and a category. |
| `Requests` | One entry per provider call, carrying usage counters and a reconstructed prompt prefix signature. |
| `ToolUses` / `ToolResults` | Tool invocations and their payloads, cross-indexed with `Events`. |

Plus the session's own metadata, the count of lines that could not be decoded, and
the runtime's capability profile expressed indirectly through which fields are
populated.

### Three fields that carry most of the design

**`Event.Transient`** marks content that entered exactly one prompt and was then
dropped by the runtime, rather than carried forward. This looks like a detail and
is not: it changes a piece of context's cost by the number of requests that
followed it. Claude Code's transcript is append-only so nothing is transient;
Kiro discards file-write bodies after the call that produced them. Charging those
bodies to every later request overstated one real session by roughly 3x before
this existed.

**`trace.Amount`** pairs a token count with the `Method` that produced it, and
derives a four-level `Grade` from it:

| Grade | Methods | Meaning |
| --- | --- | --- |
| `exact` | `provider-usage` | Read verbatim from provider usage counters. |
| `estimated` | `heuristic-tokenizer`, `calibrated-estimate` | Counted by the offline tokenizer, optionally calibrated. |
| `inferred` | `residual`, `scaled-context-usage` | Derived: a measured total minus an estimate, or a reported percentage scaled by an assumed window. |
| `unknown` | `unknown` | Not determinable. |

The grade is *derived* from the method rather than stored alongside it, so the two
cannot contradict each other. A report never has to guess how confident to be
about a number: the number says.

**`trace.OptInt` / `OptFloat`** are integers and floats that may legitimately be
unknown, and they marshal to `null` rather than `0`. This is the mechanism that
keeps `unknown` from quietly becoming a plausible zero somewhere between the
analyzer and the JSON. If you find yourself unwrapping one with `.Or(0)` for
anything other than a bar-chart width, that is usually a bug.

### Who wrote each message

The questions section splits a session at every message a person wrote, and only an
adapter can tell those apart from what the runtime, a hook or a workflow put into
the conversation. So the classification travels in the trace, and no analyzer reads
message text to decide:

| Field | What it says |
| --- | --- |
| `Trace.Prompts` | Whether the adapter classified its user-role events (`classified`), or the prompts are absent from what was read (`missing`, as for a Kiro CLI session rebuilt from its sidecar alone). Anything else means the split is unavailable and the whole session is one bucket. |
| `Event.Origin` | For a user-role event: `prompt`, `command` (a slash command) and `shell` were typed by a person and open a question; `preamble` is the wrapper a runtime writes in front of every prompt (Claude Code's `<local-command-caveat>`, Codex's `<environment_context>`); `injected` is everything else, from tool results to reminders, hook output, steering and compaction summaries. |
| `Event.Display` | What a person would recognise, for the one-line preview: injected wrappers removed, a slash command as `/name args`, never command output. Never serialized. |
| `Event.Signal` | An interruption or a failed turn the runtime recorded. Set independently of `Origin`, and attributed to the question it happened in. |
| `Event.SubagentStart` | A subagent started here whose requests this transcript does not contain. |
| `Trace.Turns` | The runtime's own turns (`RuntimeTurn`), where it records them: the event range, status, duration and, for a runtime that bills in its own units, the turn's charge. `Start = -1` means the turn could not be placed in the transcript and its counts sit on a request chosen by position. |
| `Trace.UnreportedUsage`, `UsageMismatch`, `UnreadableUserRecords`, `CreditsUnavailable` | A usage class the runtime never reports (Codex has no cache-write counter), per-request usage that does not add up to the runtime's own running total, user records that could not be decoded, and why credits are unknown. Each lowers the split's reliability or explains a dash, rather than reading as zero. |

## Adapters

`adapters.Detect` identifies the runtime structurally rather than by filename
where possible. The five shapes are mutually exclusive:

| Runtime | Shape |
| --- | --- |
| Kiro (IDE) | `{id, timestamp, payload}` |
| Kiro CLI | `{version, kind, data}` — no `payload`, no top-level `type` |
| Codex CLI | `{timestamp, type, payload}` with a second type *inside* the payload |
| Claude Code | record fields at the top level with one `type` |
| Cursor | not JSONL at all: a SQLite database with a `cursorDiskKV` table |

Detection therefore does not depend on a path convention that a future version
might change. Claude Code is the fallback, so every other runtime has to be probed
before it; an adapter that forgets to add itself is silently parsed as Claude Code
and produces a plausible but wrong report, which is why
`TestDetectRoutesEachRuntimeToItsOwnParser` covers every kind.

Cursor is the exception to "one file, one session": it keeps every conversation in
one database, so a session is addressed as `state.vscdb#<composerId>`. Handing over
a bare database path selects the largest conversation and says so, rather than
blending unrelated chats into a trace describing a conversation that never
happened.

Each adapter also publishes a `Capabilities` profile, which is how the report
decides whether a section is meaningful or should say "this runtime does not say":

The table is by runtime rather than by capability because the interesting reading
is down a row: what one runtime will and will not tell you.

| Runtime | Token usage | Cache reads | Cache writes | System prompt | MCP origin | Context window |
| --- | --- | --- | --- | --- | --- | --- |
| Claude Code | yes | yes | yes | no | no | no |
| Kiro (IDE) | no, bills in credits | no | no | yes | yes | as a percentage |
| Kiro CLI | no, bills in credits | no | no | no | no | percentage **and** stated size |
| Cursor | no, bills in requests | no | no | no | no | no |
| Codex CLI | yes | yes | no | yes | no | as a size |

The first four runtimes are close to complementary, which is the useful part. Claude
Code gives exact tokens but hides the system prompt, so `System / Rules` has to be a
residual. Kiro stores the system prompt but gives no tokens, so everything is an
estimate and the cost has to be modelled. Codex reports tokens and its window but
cannot distinguish an MCP tool from a built-in, so the MCP section may undercount.

Two rows deserve reading closely.

**Kiro CLI is not Kiro.** Same vendor, unrelated on-disk format, and a different
profile, which is why it is a separate adapter rather than a branch. Its per-turn
records *have* `input_token_count` and `output_token_count` fields and they are
`0` on observed sessions while the same turn reports a real credit charge and a real
utilization — a turn measured at 2.2753% of a stated 200,000-token window is roughly
4,550 tokens, reported as zero. So the capability says no: declaring exact token
accounting and then printing zeros is the specific failure this project exists to
avoid. The adapter still reads the fields and promotes them to measured usage when
they are non-zero. What it does have that the IDE does not is a **stated** window, so
utilization converts against a reported number rather than an assumed one.

**Cursor tells us nothing numeric.** No tokens, no cache, no cost, no window, no
utilization. It is the only runtime where nothing can anchor or cross-check an
estimate, so every figure in a Cursor report is AIProbe's own and the report says so
in a warning rather than in a footnote. What survives intact is the content analysis:
repeated context, files read twice, oversized tool results and bloated tool schemas
are conclusions about what is in the conversation, not about what was billed, and
they do not need a provider total to be true.

The trace model has to make all five honest, which is why capabilities are data rather
than `if adapter == "kiro"` scattered through the analyzers.

**Who wrote each message is the adapter's call.** Each applies its own rules, first
match wins, and records the answer as `Event.Origin`:

- *Claude Code:* tool results, `isMeta` records, compaction summaries and injected
  wrappers are injected. Text that *starts* with `<command-name>` or
  `<command-message>` is a slash command (a tag in the middle of a prompt is still a
  prompt), `<bash-input>` is a shell command, and `[Request interrupted by user]` is
  a signal on the question it interrupted. A queued prompt (a `queued_command`
  attachment) opens a question where it was delivered. A transcript that is all
  sidechain records is a subagent's own, so its first user message opens question 1.
  One API response written as several lines, one per content block, is one request:
  lines sharing `message.id` (or, without one, `requestId`) are merged. Later lines
  still add their events and tool calls to that request, a later line whose usage
  differs replaces the stored usage, and the first line's event anchors it. Earlier
  builds counted every line as a request, so affected sessions show roughly half the
  requests, billed input and cost they used to.
- *Codex:* `<environment_context>`, `<user_instructions>` and AGENTS.md
  instructions are preamble; `<turn_aborted>` is injected and interrupts the question
  before it; `<user_shell_command>` is a shell command; a role other than user,
  assistant, system or developer is injected. `task_started`, `task_complete` and
  `turn_aborted` become runtime turns, and the assistant a `token_count` would attach
  to is reset at each turn boundary, so a request after an aborted turn does not land
  on the turn before it.
- *Kiro:* a user message from steering, one carrying a `_meta.kiro` notification or
  synthetic reason, or one with only context items is injected. `turn_start`,
  `usage_summary` and `turn_end` become runtime turns with their credits.
- *Kiro CLI:* every `Prompt` record is a prompt. The sidecar's turns become runtime
  turns, placed by `message_ids`, then `result.Ok.ID`, else `Start = -1` — the same
  order the token counts are placed by, so "unplaced" means exactly "counts on a
  guessed request".
- *Cursor:* a user bubble with text is a prompt, an empty one is injected.

New transcript fields are read as `json.RawMessage` and decoded only in the branch
that needs them, so a field of an unexpected shape counts as missing instead of
costing the whole line. Several of these branches have not been observed on real
sessions here (Claude Code's `<bash-input>`, the interruption variants and old-format
system commands; Codex's `turn_aborted` and its two wrappers; every Kiro CLI rule)
and follow the format's code and synthetic samples. The standing limitation is that a
marker a runtime starts writing tomorrow reads as a person's prompt until a rule is
added for it; the reliability line is where that would first show.

## Token estimation, and keeping it honest

`internal/tokenize` is a character-class heuristic, not a real tokenizer: no
vocabulary file, no model download, no network. It is deliberately approximate and
everything it produces is marked `estimated`.

Two mechanisms stop that approximation drifting into fiction.

**Calibration.** When a session has consecutive requests with measured usage, the
growth in measured prompt tokens between them is compared with the estimator's
count of the content added in between. The ratio, taken as a median over all such
pairs, scales subsequent estimates. The demo session reports `×1.00` because no
correction was needed. Calibration requires at least two samples; below that the
raw values are used and a note says so.

**Cross-checking.** A runtime with no token counters cannot be calibrated, so
something else has to falsify the estimate. Both Kiro variants report context-window
utilization per turn, and `kiro.Check` compares the estimate against that curve. The
CLI adapter reuses the same function rather than copying it, so the two cannot drift
apart; what differs is what the answer means, and the CLI's case is the stronger one
because it states its window instead of leaving it to be inferred.

Cursor reports no utilization either, so it gets neither calibration nor a
cross-check. That is stated in the report rather than papered over.

There is one more thing the Kiro CLI pairing makes provable. The CLI does not persist
the system prompt or the tool schemas, so the breakdown has no `System / Rules`
content and would otherwise imply the fixed per-request overhead is zero. It is not,
and unusually this is arithmetic rather than argument: the runtime states both how
full the window is and how big the window is, and the difference between that product
and everything the transcript accounts for is content that was sent but not stored.
The adapter reports that gap with its numbers. It does not turn it into a breakdown
row, because splitting it between the system prompt and the tool schemas is not
something the file supports.

The subtlety is that a percentage only becomes a token count if you know the
window, and Kiro never says what the window is. Fitting the window to the data
would force agreement and prove nothing. So the check reports two
window-independent statistics instead:

- **Tracking** — the p90/p10 spread of the implied window across samples. Near 1.0
  means the estimate is proportional to the runtime's own accounting, whatever the
  window is.
- **Implied window** — the window the pairing implies. An *output* of the check,
  never an input.

Attribution is only called trustworthy when both the shape and the scale agree.
Shape alone is not enough: an estimate that tracks perfectly but is systematically
2x too large still produces wrong per-category token counts.

## Analyzers

Each is independent and each declines to guess.

**`context`** attributes every billed prompt token to one of seven categories,
*cumulatively*: content is counted once per request that had to carry it, which is
what makes a 6k file read on turn 3 of a 40-turn session cost ~220k rather than
6k. `System / Rules` is what is left after subtracting estimated visible context
from the measured total — a residual, and marked as `inferred` rather than
presented as measured.

**`mcp`** costs tool schemas from an optional local inventory. Tool schemas are
never written to a transcript, so without an inventory the loaded tool count,
utilization and schema cost are all `unknown`. The denominator is not invented.

**`duplicate`** finds exact repeats by normalized content fingerprint and near
repeats by line-shingle Jaccard similarity above a threshold. Lexical only: no
embeddings, no model calls, so it runs in milliseconds and returns the same answer
every time. It reports insert waste (what a copy cost once) and billed waste (that
cost times the requests that carried it) separately, because only the second is
worth acting on and only the first is recoverable content.

**`cache`** takes cache counters verbatim, then explains the gaps. For each drift
event it compares the reconstructed prompt prefix of consecutive requests and
attributes a cause — compaction, model change, TTL expiry, prefix rewrite, rules
change, metadata change, tool-set change, volatile content — each with a
confidence, or `unknown` when the transcript cannot explain it. Guessing here
would be actively harmful: the fix for an idle timeout has nothing in common with
the fix for an edited rules file.

**`rules`** runs 17 deterministic rules over the other analyzers' output. Each
returns the evidence it fired on, the recoverable tokens, the change to make, and a
confidence. No model is involved.

**`score`** is a published formula over five components, with unevaluable
components reported as unknown and their weight redistributed rather than scored
as zero. See [scoring.md](scoring.md).

**`questions`** (`internal/analyzer/questions.go`) splits the session at the events
whose `Origin` opens a question and assigns every request, tool call and event to
exactly one segment: question 1..n, `before_first` (everything before the first
question) or `unattributed` (runtime turns that could not be placed). A request goes
where its anchoring event is; when that index points at the next event still to be
added, it goes with the one before, so a Codex response holding only a
`function_call` stays with the question that asked for it. Signals belong to the
question they happened in, and a runtime turn to the question of its first request.

With provider counters each request is priced on its own, from a `RateCard` looked
up once per model, and a question's cost is the sum of its requests. Without them
the session's modelled cache writes and reads are apportioned by the
largest-remainder method on two weights recorded by the context breakdown in the
same pass (`Attribution`): a question's writes weigh the context its requests sent
for the first time plus the one-off content they were charged for, its reads the
context they sent again. Writes therefore land where the content was first sent, as
a provider would bill them, and a segment with no requests gets no modelled cost.
Credits are split by runtime turn when the runtime records a charge per turn, shown
as a floor (`≥`) for a question with a turn that reported none, and kept at the
session level when only a session total exists.

Where the runtime charged each turn and the session's credits are above zero
(`billedPerTurn`), the default order and every share are taken in credits: they are
the bill, and the dollar figure beside them is AIProbe's estimate at list prices,
modelled outright when there are no counters. Otherwise the order is cost, then
context exposure, and shares follow the same basis. The basis belongs to the session,
so `--sort` reorders the rows without changing what a share is a share of.

The invariants all hold inside this one function: every request in exactly one
segment; token classes, costs and credits that sum to the session's own totals
(integers exactly, money to within 1e-9 relative); shares that sum to 1. A
reconciliation pass checks them afterwards, re-apportions an integer class that does
not add up, and says so with `questions.note.reconciled`, which is a bug report
rather than a caveat. Everything downstream copies: renderers format, and
`SummaryOf` is the one function that turns a result into the session cost row the
ranking and the store index carry. A reliability verdict with its reason travels
with the split: skipped lines, unreadable user records, a usage mismatch, unplaced
runtime turns (`turns_unplaced`, with the count), an adapter that does not
classify, or prompts that are absent from what was read.

## Turning tokens into money

`internal/pricing` holds the price table; `analyzer.Spend` holds the result. Four
decisions carry the weight, and the last two exist because the obvious
implementations were wrong in ways that got caught by looking at real output.
User-facing detail is in [pricing.md](pricing.md).

**Four rates per model, not one.** `pricing.Price` is `{Input, Output, CacheWrite,
CacheRead}`. Anthropic bills cache writes at 1.25× input and reads at 0.1×, so a
blended price-per-token is wrong by an order of magnitude on exactly the sessions
this tool exists to examine. The four-way split is also what makes the cost section
diagnostic rather than decorative: a large cache-read line is cheap and a large input
line is not, and token counts alone do not say which you have.

**The rules engine knows nothing about money.** `rules.Evaluate` stays a pure
function of the analyzers' output. The analyzer attaches costs afterwards, filling
`Recommendation.WastedCost`, so a rule cannot come to depend on a price and the
recommendations remain byte-identical whether pricing is on or off. `Options.Pricing`
being nil produces no `Spend` section at all rather than one full of zeroes, so a
consumer can distinguish "pricing was off" from "this cost nothing".

**Requests are priced individually, then grouped by model.** A session that switched
from Opus to Haiku is charged at each model's own rate. An early version attributed
the whole session to the priciest model it saw, which relabelled the demo session at
$15/M on the strength of a single throwaway request.

**Recoverable waste is valued at the session's own effective prompt rate**, computed
as `(input$ + cacheWrite$ + cacheRead$) / promptTokens`, not at the list input rate.
Most recoverable context is cache-eligible — tool schemas live in the cached prefix,
a file read three times is served from cache twice — so the session was paying a
fraction of the input rate to carry it. Valuing it at the full rate produced a claimed
saving of $5.95 against a $1.65 bill: not merely imprecise but impossible, and the
fastest way to make a reader distrust every other figure. A test asserts that
recoverable can never exceed what was spent, and per-finding costs use the same rate
so they cannot sum past the total.

Two smaller rules follow the same principle as the rest of the codebase, that a
stated unknown beats a fabricated number:

- **An unlisted model is approximated, not withheld — but never guessed at
  plausibly.** It is priced at a generic fallback and the result carries
  `SourceFallback`, which every renderer turns into a labelled caveat naming the
  model. The original design reported it as unknown; that was right about unlabelled
  numbers and wrong here, because anyone on a model newer than the table saw no cost
  at all. What is still refused is a guess that *looks* like knowledge: prefix
  matching resolves an allowlist of decorations (`-latest`, `-thinking`) but never a
  version bump, so `claude-opus-4-7` does not inherit `claude-opus-4`'s price. Those
  generations differ threefold, and that one was found in the field misreporting a
  real session by 3×. The fallback is configurable and can be switched off.
- **A runtime with no counters gets its cost modelled, and that took two attempts.**
  Kiro reports no token usage and bills in credits. The first version showed it no
  money at all, which made the feature invisible to the runtime most likely to be
  pointed at this tool. The second priced its whole cumulative context at the uncached
  input rate and called it an upper bound — technically true and practically useless,
  because it read as **$5,367 for a session that models out at $547**.

  The fix was to notice that the cache split *is* derivable. `Inserted` in the context
  breakdown is the one-off cost of content, ignoring re-sends; content is written to
  cache once and read back on every later request, so inserted is the cache-write side
  and the rest of the cumulative total is reads. On a real session that is 99.8% reads,
  which is exactly why the uncached reading was off by ten.

  `modelledUsage` synthesises a `trace.Usage` from those two numbers and hands it to
  the same pricing path a measured session uses, so the class breakdown, the recoverable
  figure and the per-finding costs all work without special cases. What it does not
  model is cache expiry, which makes the figure a floor; `Spend.Ceiling` keeps the
  all-uncached reading as the matching roof and both are shown. The runtime's own credit
  total rides alongside and neither is converted into the other.

Cost inherits the provenance of its tokens. `pricing.Cost.Estimated` is set from the
caller's own flag, and the renderers mark an estimated cost with the same leading `~`
they use for an estimated token count. The recoverable figure is always estimated,
because the waste behind it is analysis rather than billing data, even when the
session's own cost came from provider counters.

## Aggregating across sessions

`internal/analyzer/overview` folds many `analyzer.Result` values into one report.
The important decision is that it aggregates *results*, not sessions.

Concatenating every transcript into one trace and running the normal analyzers
would have been less code, and would have been wrong in a way that is hard to
notice. Two analyzers are only meaningful inside one conversation:

- **Duplicate detection** would treat a file read in session A and again in session
  B as redundancy. It is not: each session needed its own copy, because they are
  separate contexts. The aggregate would report waste that no change in how you
  work could recover.
- **Cache attribution** compares the prompt prefix of consecutive requests. Across
  a session boundary the prefix legitimately differs, so every boundary would
  appear as a cache miss with no explicable cause.

So each session is analysed exactly as it would be on its own, and the aggregate
sums and ranks the outputs. Every number in the overview therefore means the same
thing it means in a single-session report, which is what makes the two comparable.

Aggregation introduces one new honesty problem of its own. Summing a quantity that
is unknown in some sessions produces a **floor, not a total** — a session whose MCP
cost could not be measured contributes zero, and a naive total would read as though
that session were clean. So `overview.Sum` carries the count of sessions it could
not measure, `Complete()` reports whether any are missing, and both renderers mark
an incomplete sum (`≥` in the terminal, "at least" in HTML) rather than presenting
it as complete.

For the same reason scores are reported as a distribution — median, best, worst,
grade histogram — rather than averaged into a single number that would look like
one session's grade.

Money gets the same treatment through `overview.MoneySum`, which is `Sum` for
currency: it carries how many sessions it could not price, and the renderers mark an
incomplete total the same way. It adds one rule of its own — a set whose sessions used
different currency labels is **not** summed, because converting between currencies
needs a rate the tool has no honest way to obtain, so the differently-denominated
sessions count as unpriced and the report says so.

The cost ranking (`Report.Ranking`) lists every session the aggregate covered,
regardless of `--top`. Each row's figures are the session's own cost row from
`analyzer.SummaryOf`, copied rather than recomputed, so a row always equals that
session's report, and `SortRanking` is the one comparator every surface uses. Its
default, `DefaultRankingSort`, is credits for a set from one runtime with a session
charged per turn, since those credits are what that runtime billed and the order then
matches each session's own questions; otherwise money when any session has a
comparable cost, else credits, else context exposure. A set mixing runtimes has no
common unit but money, so it stays on cost, and `RankingModelled` has the renderers
say beside the table that the `~` amounts on per-turn rows are estimates, whatever the
order. A row priced in another currency or at another discount than the caller's
`PriceRef` is marked and not ranked by cost against the others; with pricing off the
rows carry no money at all. Session titles are scrubbed once per session (credentials redacted,
control characters removed, 80 runes at most) and the same string is used by the
ranking, the worst-sessions table and the finding examples.

The aggregate is the only thing the local server caches, because it is the only
thing that parses every session rather than one. The cache key is the runtime plus
a fingerprint of the session set, and it is deliberately **not** keyed on language:
analyzers record message ids rather than sentences, so one aggregate renders in any
locale and switching language costs nothing.

It is also the only thing slow enough to need reporting on. Analysing a session
costs about a second, so a Cursor store with 38 conversations is a minute, and there
is no ceiling on how large a history gets. Streaming progress into a held-open
request was rejected for two reasons: a request open for ten minutes is at the mercy
of every timeout between the browser and the handler, and two tabs on the same
overview would analyse everything twice. So the work is a job, keyed exactly the way
the cache is keyed, and the browser polls it — `POST /overview/start` then
`GET /overview/progress`. Nothing is held open for longer than one poll interval, a
second caller joins the first, and closing the page leaves the work running.
`GET /overview` still blocks for a direct URL or a script; it waits on the same job.

That is also why `serve` sets no `WriteTimeout`: the blocking route has no honest
upper bound, and a truncated page is indistinguishable from a crash.

## Persisting analyses

`internal/store` keeps every analysis on disk. Two reasons, and the second is the
one that mattered:

1. Without it the local server re-parsed and re-analysed on every page load.
   Opening a report, switching language and pressing back cost three full analyses
   of the same unchanged bytes — about 190 ms each on a real session, 8 ms from the
   store.
2. There was no such thing as an analysis history, so "is my context usage getting
   better?" had no answer even though every run had already computed it.

### The cache key has to include the build

`store.Fingerprint` hashes the session path, its size, its mtime and the **tool
version**. The version is what makes an upgrade re-analyse instead of replaying an
older build's conclusions, and leaving it out would make every improvement invisible on
any session already in the history.

That only works if the version actually changes. It did not: with no git tags in the
tree, `git describe` failed and every build called itself `0.1.0-dev`, so a stored
analysis from an older build matched the fingerprint exactly. The symptom reached a
user — a report kept rendering "cost unknown" for a session the current binary priced at
$10.64, because the served page came from the cache. `make build` now appends a build
stamp to any non-release version, and `buildVersion` falls back to Go's embedded VCS
stamp for `go install`.

`Server.usable` is the belt to that braces: it rejects a stored analysis whose spend
section is empty in a way the current build would never produce. It exists because the
version scheme is a convention and conventions get broken, and because the failure mode
is silent.

The question it asks is whether the stored spend section is something *that runtime*
could have produced — a section that says nothing, or that says something the runtime is
incapable of saying, came from a different build. Asking it less precisely broke the
store outright. An earlier version accepted only "no price on file for this model" as a
legitimate reason for a missing figure, which meant every other honest answer read as
stale. Cursor reports no token counters at all, so every Cursor session has an empty
spend with reason `no_token_counters` and none of them were ever reused: the store held
38 Cursor entries and used none, and the overview paid its full minute every time it was
opened rather than only the first. A guard against serving the wrong answer had turned
into a guarantee of doing the slow thing forever.

### The stored artifact is the report

```
~/.aiprobe/
├── index.json            one record per analysis: identity, timing, headline figures, cost row
└── analyses/<id>.json     the full report, byte-identical to `--json`
```

`index.json` duplicates the headline figures so listing the history does not have
to open several hundred files. The analysis files themselves go through
`report.WriteJSON`, the same function `--json` uses, so there is no second
serialization format to keep in step and no stored document that a reader of the
JSON report cannot already parse. `WriteJSON` drops question previews
unconditionally, which is why nothing in the store ever holds one.

Each record also carries `summary`, the session's cost row from
`analyzer.SummaryOf`, so the server's Sessions and History pages show cost, credits
and question count from the index alone, without opening a stored report or parsing
a transcript. A record written before it existed has none, and those pages say so as
"— (earlier version)", told apart from "— (older analysis)" (the session's
`SessionKey` has entries, but none for this file as it is now and this build) and
"— (not analysed)". `history --json` leaves it out, so that output is unchanged.

Two cases the questions section added. A stored analysis from before it, under the
same version string, is still reused, because `usable` does not look at it: telling
"older than this section" from "this session has no questions" by the data alone is
how a server re-analyses the same session on every request. The report page says so
instead and offers a button that POSTs to `/report/reanalyse`. The one-click
aggregate is the exception, because it was asked for every session's questions:
`analyseWith(sess, true)` treats a stored analysis without them as stale and replaces
it under the same fingerprint, so the refresh happens once and the next aggregate
reuses the result. The CLI `overview` never reads the store, so it has nothing to
refresh. And `serve --no-cost` does not reuse a stored analysis that carries a
cost, since showing it would put money on a page that promised none: it analyses
afresh without prices and does not write the result back, so a priced history entry
is not erased by one unpriced visit.

### Identity: what makes a cache hit, and what makes a trend

Two keys, doing different jobs:

| Key | Made of | Job |
| --- | --- | --- |
| `Fingerprint` | session path, size, mtime, **tool version** | is this exact analysis already done |
| `SessionKey` | adapter, session path | which analyses are of the same session |

The tool version is in the fingerprint on purpose: an analysis produced by an older
build is not comparable with one from this build, so it has to be re-run rather
than served. `Entry.Stale` lets the history page say so instead of charting the two
together.

Because the fingerprint includes size and mtime, re-analysing an *unchanged*
session replaces its entry, while a session that has grown adds one. That
asymmetry is what produces a trend without accumulating identical rows: the
history page shows one row per session and the repeated analyses become the
sparkline on it.

### Why files rather than SQLite

The queries are: fetch one analysis by fingerprint, list newest first, filter by
session. Three of them, over a few hundred records. None wants SQL.

The measured cost of the alternative decided it. `modernc.org/sqlite`, the pure-Go
driver, takes the module graph from 1 dependency to 26 including a C-to-Go
translation of libc; the cgo driver instead breaks the pure-Go cross-compilation
the release builds depend on. For a tool that reads people's prompts and asks to be
audited, that is a large increase in what a reviewer has to trust in exchange for
avoiding a hundred lines of file handling.

Files also keep a property worth more than query flexibility here: the history is
readable and deletable with a text editor, and it needs no tooling to inspect.
`internal/store` is one package behind one interface, so if a query arrives that
files genuinely cannot serve, the reports remain the source of truth to migrate
from.

### Reading SQLite without a SQLite driver

That decision held, and then Cursor forced the question from the other side.
Cursor keeps chat history only in a SQLite database, so supporting it meant either
taking the dependency after all or reading the file format directly.

`internal/sqlitedb` reads the bytes. It is deliberately not a SQLite
implementation: no SQL, no query planner, no write path, no indexes, no
transactions. The whole surface is "walk one table's rows", which is all an adapter
needs. What it does implement is what the format requires to read real files — the
header, the `sqlite_master` catalogue, table b-tree traversal for interior and leaf
pages, the record format with every serial type, overflow page chains, and the read
half of write-ahead-log recovery. Overflow is not optional: a chat message is
routinely larger than a page, so ignoring it would silently truncate exactly the
rows worth reading.

The write-ahead log is not optional either, which took a bug report to establish.
A database in WAL mode keeps recently committed pages in a `-wal` sidecar until a
checkpoint folds them in, and Cursor holds its store open for as long as the editor
runs, so the global store is in WAL mode permanently and the newest conversations
live partly in that file. Refusing WAL databases — the original behaviour — was
honest but covered every Cursor installation that had ever been used: `aiprobe
runtimes` reported "no sessions" on machines with hundreds of them. So the sidecar
is recovered by scanning it, the same way SQLite does when no other connection has
built the shared-memory index: salts, the cumulative Fibonacci-weighted checksum
chain, and commit markers decide which frames are live, and the resulting pages
override the main file. A frame whose checksum breaks the chain ends the scan rather
than failing the read, so a writer appending while this runs leaves a torn tail
behind instead of an error.

Two things make the whole approach defensible rather than reckless.

**It refuses rather than guesses.** A `-wal` sidecar that exists but cannot be
interpreted is an error with an explanation, not something to ignore: ignoring it
would report the main file as though it were complete. Anything the format says is
impossible — a cell pointer outside its page, a cyclic b-tree, a truncated record,
an index page where a table was expected — is also an error. A reader that skipped
unreadable pages would report a smaller session instead of a failure, which is the
worse outcome. When the sidecar is simply absent the main file is read as it
stands, which is what SQLite itself does, since a clean shutdown checkpoints the
log and deletes it.

**It is verified against SQLite's own output.** The fixtures under
`internal/sqlitedb/testdata` are written by the `sqlite3` CLI, not by this package,
because a parser checked only against its own output proves nothing. They include a
26,800-byte value spanning about fifty overflow pages, compared byte-for-byte by
digest; a 400-row table over 512-byte pages so the b-tree really has interior levels
above its leaves, alongside index pages the reader must refuse; and every serial
type the record format defines.

The store still uses files. Nothing about reading someone else's database changes
what this tool should write.

### Failure behaviour

- Writes go through a temporary file, `fsync`, `chmod 0600` and a rename, so an
  interrupted run cannot leave a half-written index or a partial report.
- A damaged `index.json` degrades to an empty history rather than an error. History
  is a convenience; refusing to analyse would be a regression in the actual job.
- The history is capped (500 by default) and eviction removes the analysis files
  too, so a local diagnostic cannot grow without bound in someone's home directory.
- The bundled demo is never stored, for the same reason the aggregate excludes it:
  synthetic figures in a real trend would be fiction presented as measurement.

## From a finding to something to do

`internal/analyzer/rules` detects. `internal/analyzer/rules/advice.go` says what to
do about it. Keeping those apart matters more than it looks.

Advice is resolved **by rule id**, not written in the rule body. `Evaluate` derives
`rule.RNNN.step.1`, `.step.2` and so on, counting up until a catalog entry is
missing. Two consequences:

- A rule becomes more actionable by adding catalog entries. Nothing in the detection
  logic changes, so improving advice cannot break a threshold.
- The step count is derived from the **English** catalog, so a translation missing
  one entry shows that step in English rather than silently producing a shorter list
  in one language than another.

Tool hints work the same way, with identity as data and prose as message ids:

```go
"rtk": {
    Name:      "rtk",
    Install:   "cargo install rtk  # …",
    DocRef:    PlaybookRef + "#rtk",
    WhatMsg:   Msg{ID: "tool.rtk.what"},
    VerifyMsg: Msg{ID: "tool.rtk.verify"},
},
```

Three rules govern what may go in there.

**Every hint carries a verification.** Recommending an install with no way to check
it worked is how a tool list becomes a link farm. Every `VerifyMsg` names a
measurement this tool makes, so the loop closes: analyse, change one thing, analyse
again.

**No URLs.** `DocRef` is a relative repository path. The HTML report is
self-contained and `TestHTMLReportIsSelfContained` greps the rendered bytes for
`https://` with no allowlist, so links live in the playbook and reports point at the
playbook. `TestPlaybookReferencesResolve` then checks each path lands on a heading
that really exists — a report pointing at a dead anchor is worse than one pointing
nowhere, and nothing else would notice.

**Most rules get no tool.** Nine of seventeen do. MCP configuration, model choice
and cache hygiene are settings to change; attaching a tool to all of them would
devalue the ones that mean something.

The cross-session aggregate resolves advice from the rule id through
`StepsByRule`/`ToolsByRule` rather than carrying a copy, so `overview.FindingTotal`
and its JSON shape are unchanged.

## Reporting and localization

All three renderers consume the same `analyzer.Result`. The JSON output is the
contract; the terminal and HTML views are presentations of it.

The renderers are the *only* layer that turns data into sentences. Analyzers run
before anyone knows what language the report will be in — `aiprobe serve`
renders one analysis in two languages from a single run — so an analyzer that wants
to say something records an `i18n.Msg`: a message id plus its arguments. The
reporter renders it in the target locale.

This has a second benefit. The English string the JSON report carries is rendered
from the same catalog entry as the translated one, so the two cannot describe
different things. There is no English prose in any analyzer package.

Two consequences worth knowing:

- Placeholders are named (`{tokens}`), not positional. Chinese and English order
  clauses differently, so a translator has to be able to move a value.
- A sentence that embeds a translatable term — a cache drift cause inside a rule's
  evidence line — passes it as an `i18n.Ref` or a nested `i18n.Msg`, resolved at
  render time. Concatenating the rendered term in the analyzer would pin the word
  order to English.

The terminal renderer derives every column width from the configured total width
and measures strings in display cells rather than runes, because CJK glyphs are
double width. A test asserts no line overflows at several widths in both locales.

The HTML renderer emits one self-contained file: inline CSS, inline JavaScript,
SVG charts generated in Go. No CDN, no web font, no analytics. CI greps the
artifact for external URLs rather than trusting the claim.

## Things that are enforced, not merely intended

| Property | Enforced by |
| --- | --- |
| No network client in the binary | `TestNoForbiddenImports` walks the import graph; `internal/serve` is the only exception |
| `serve` binds loopback only | `TestNonLoopbackHostIsRefused` — including that an unresolvable hostname is refused rather than assumed safe |
| Served pages fetch nothing | `TestServedPagesReferenceNoExternalOrigin`, `TestHistoryReferencesNoExternalOrigin` grep the rendered bytes |
| The progress polling reaches this server and nowhere else | `TestPolicyAllowsPollingThisServer` — `connect-src 'self'`, which the pages need and which nothing widens |
| A session title from a transcript never becomes markup | `TestOverlayScriptIsSafeAndSelfHealing` — the overlay must not assign `innerHTML` |
| Session links survive their own escaping | `TestSessionLinksSurviveTheirOwnEscaping` — a Windows id contains backslashes, and escaping it twice made every button 404 |
| Reading progress starts no work | `TestPollingDoesNotStartWork` |
| A stored analysis is reused unless the runtime could not have produced it | `TestUsableRejectsOnlyWhatItShould` — too strict here re-analysed every Cursor session forever |
| Findings carry no URLs, only repository paths | `TestToolHintsCarryNoURLs` |
| A saving is never larger than the spend it came out of | `TestRecoverableNeverExceedsWhatWasSpent`, `TestFindingCostsAreConsistentWithTheTotal` |
| Waste is priced at what caching actually cost, not at list input | `TestEffectiveRateReflectsCaching` |
| An approximated cost is labelled, and a version bump inherits no price | `TestUnlistedModelIsApproximatedAndSaysSo`, `TestVersionBumpsAreNotPricedByPrefix` |
| Two numbers are enough to configure a model, and an explicit zero survives | `TestConfigNeedsOnlyInputAndOutput` |
| The generic rate can be switched off, restoring the explicit unknown | `TestFallbackCanBeSwitchedOff` |
| A counter-less runtime gets a modelled figure, marked estimated, under its own ceiling | `TestNoCountersProducesAModelledCost`, `TestModelledSessionCarriesItsCeiling` |
| A modelled figure applies the cache split rather than the uncached rate | `TestModelledCostIsNotTheUncachedReading` |
| A credit total is never used as a dollar total | `TestCreditBilledRuntimeKeepsItsCreditsAndGetsAFigure` |
| A measured session grows no second "at most" number | `TestMeasuredSessionHasNoCeiling` |
| The rates are reachable from every page of the local server | `TestPricingIsReachableFromEveryPage` |
| A cached analysis from a different build is never served | `TestEmptySpendFromTheSameVersionIsReanalysed`, `TestVersionIsSpecificEnoughToInvalidateACache` |
| `pricing --init` cannot destroy rates you typed in | `TestWriteRefusesToClobber` |
| Every playbook reference in a report resolves to a real heading | `TestPlaybookReferencesResolve` |
| Every rule has executable steps, in every locale | `TestEveryRuleHasExecutableSteps`, `TestStepsExistInEveryLocale` |
| Every tool hint says how to verify it helped | `TestEveryToolHintCanBeVerified` |
| Both stylesheets define the same light palette | `TestThemesAgreeAcrossStylesheets` |
| Every page offers the theme toggle, applied before first paint | `TestEveryPageOffersTheThemeToggle` |
| A new adapter is wired everywhere, not just into `Kinds` | `TestEverySelectableKindIsFullyWired`, `TestDetectRoutesEachRuntimeToItsOwnParser` |
| A runtime reporting no tokens has to name what it bills in | `TestEverySelectableKindIsFullyWired` |
| The SQLite reader matches what SQLite itself wrote, overflow included | `TestOverflowChainIsFollowedExactly`, `TestInteriorPagesAreTraversed`, `TestEverySerialTypeDecodes` |
| A WAL database is read through its log, not read stale | `TestWALSidecarRowsAreRead`, `TestWithoutItsLogTheSameFileIsStale`, `TestConversationsLivingOnlyInTheWriteAheadLogAreFound` |
| A log that cannot be interpreted is refused, not ignored | `TestUnreadableSidecarIsRefused` |
| A writer appending mid-read leaves a torn tail, not an error | `TestPartialTailFrameIsIgnored`, `TestStaleFramesAfterACheckpointAreIgnored` |
| Corruption surfaces as an error, never as a shorter session | `TestCorruptionIsAnErrorNotASilentShortRead` |
| A two-file session is not double counted from its own sidecar | `TestSidecarAssistantCopyIsNotCountedTwice` |
| Present-but-zero token counters are not presented as measured | `TestZeroTokenCountersAreNotTreatedAsMeasured` |
| One database holding many chats never silently blends them | `TestBareDatabasePicksTheLargestAndSaysSo`, `TestNamedConversationIsSelectedExactly` |
| An unreadable tool record is counted, never invented into a nameless tool | `TestUnreadableToolRecordIsCountedNotInvented`, `TestUnreadableToolRecordDoesNotInventATool` |
| Paths compare the same way in every adapter, on every platform | `TestSameDirIgnoresSpelling`, `TestSameDirCaseFollowsThePlatform` |
| Tests redirect the home directory on the platform's own variable | `TestHomeEnvVarMatchesWhatUserHomeDirReads` |
| Colour is decided once, and never written into a pipe | `TestColorEnabledRespectsEveryOptOut`, `TestSessionListIsPlainWhenNotATerminal` |
| A report is never a dead end | `TestReportOffersAWayBackToTheSessionList` — it must offer a way back and must not claim to be a navigation destination |
| Running the tests does not touch your history | `TestAnalysesAreNotStoredDuringTests`, `TestDemoIsNotRemembered` (which includes a positive control, so it cannot pass by storing being broken) |
| The root main.go stays a delegation and nothing more | `TestRootMainStaysTrivial` |
| The published scoring doc matches the code | `TestDocMatchesImplementation` |
| The score is reproducible | `TestDeterministic` |
| READMEs and `docs/demo-report.txt` quote real output | `TestReadmeHeadlineMatchesDemo`, `TestTranslatedReadmesStayInStep`, `TestCommittedDemoReportIsCurrent` |
| Every message id used actually exists, in every locale | `TestLiteralMessageIDsExist` |
| Locales cover the same ids with the same placeholders | `TestEveryLocaleCoversEnglish`, `TestNoLocaleInventsIDs`, `TestPlaceholdersMatch` |
| The cross-check does not depend on the assumed window | `TestWindowIndependence` |
| `aiprobe demo` finishes in under five seconds | `TestDemoIsFast` |

If a change trips one of these, the fix is almost never to relax the test.
