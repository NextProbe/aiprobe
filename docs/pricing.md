# Prices and costs

*[中文版](pricing.zh-CN.md)*

AIProbe turns token counts into money so the findings are easier to act on. This
document says exactly where those numbers come from, what they assume, and how to
replace them with the rates you actually pay.

The short version: token counts come from your provider, prices ship with the tool
and are probably slightly wrong for you, and anything priced with a generic rate says
so. Correcting a rate takes two numbers.

## Contents

- [Four rates, not one](#four-rates-not-one)
- [Where the built-in prices come from](#where-the-built-in-prices-come-from)
- [The generic rate](#the-generic-rate)
- [Using your own rates](#using-your-own-rates)
- [Discounts](#discounts)
- [What "recoverable" is worth](#what-recoverable-is-worth)
- [When the cost is unknown](#when-the-cost-is-unknown)
- [When there are no counters](#when-there-are-no-counters)
- [Currencies](#currencies)
- [Turning it off](#turning-it-off)
- [Reading the JSON](#reading-the-json)

## Four rates, not one

Every model carries four prices, because providers charge four different amounts for
the same request:

| Rate | What it covers |
| --- | --- |
| input | Prompt tokens sent uncached, at full price. |
| output | The completion, including reasoning tokens where the provider counts them as output. |
| cache write | Creating a prompt cache entry. Anthropic charges 1.25× the input rate for this. |
| cache read | A cache hit. Anthropic charges 0.1× the input rate; OpenAI charges 0.1× and does not bill separately for creating the entry. |

A single blended price-per-token would be wrong by an order of magnitude on exactly
the sessions this tool is most interested in. A long-running agent session is mostly
cache reads: a Claude Code session carrying 700k tokens of context might pay $0.14
for 460k cache reads and $1.49 for 220k cache writes. Collapsing that into one
average rate destroys the distinction between "your context is large but cached" and
"your context is large and you are paying full price for it", which is the difference
between a healthy session and a broken one.

So `aiprobe pricing` shows four columns, and the cost section breaks the bill into
four lines.

## Where the built-in prices come from

They are published list prices, recorded at a fixed date. That date is printed
everywhere a cost appears, and is available as `list_prices_as_of` in the JSON.

The table is deliberately short. Around a dozen models are included: the Claude and
GPT families that the supported runtimes actually name. A long table of
half-remembered numbers would be worse than a short one plus an honest unknown,
because every wrong entry is a wrong invoice figure that nobody will check.

Two things follow from prices being list prices:

- **They are not your invoice.** Enterprise agreements, committed-spend discounts,
  Bedrock and Vertex markups, and batch or priority tiers all move the real number.
  Check a total against a real bill before quoting it.
- **They go stale.** Providers cut prices regularly. If the date shown is old
  relative to the last price change you remember, override the rates.

```console
$ aiprobe pricing
 Token prices
 ────────────────────────────────────────────────────────────────────────────
  What token counts get multiplied by. Four rates per model, because
  providers charge four different amounts and a cache read can cost a tenth
  of an input token. Every rate below is what reports actually use, after
  any discount.
  Model                        Input    Output  Cache wr  Cache rd  From
  claude-haiku-4-5           $1.00/M   $5.00/M   $1.25/M  $0.100/M  built-in
  claude-opus-4             $15.00/M  $75.00/M  $18.75/M   $1.50/M  built-in
  claude-opus-5              $5.00/M  $25.00/M   $6.25/M  $0.500/M  built-in
  claude-sonnet-4-5          $3.00/M  $15.00/M   $3.75/M  $0.300/M  built-in
  gpt-5-codex                $1.25/M  $10.00/M         —  $0.125/M  built-in
  ...

  (anything else)            $3.00/M  $15.00/M   $3.75/M  $0.300/M  built-in

  A model not listed above is priced at the generic $3.00/M rate, and every
  figure from it is labelled an approximation naming the model. Set
  "default" in the price file to change it, or to 0 to report those costs as
  unknown instead.
  Built-in prices are published list prices as of 2026-09. Check them
  against your own invoice before trusting a total.
  No discount applied: the rates above are list prices.
  No price file yet. Run `aiprobe pricing --init` to write one at
  ~/.aiprobe/pricing.json, then edit the rates you actually pay.
  A version this table has never seen never inherits a neighbour's price:
  claude-opus-4-7 does not get claude-opus-4's rate. It gets the generic
  rate above, flagged as an approximation.
```

An em dash means the provider has no such rate — OpenAI does not bill separately
for creating a cache entry — rather than a rate of zero. `(anything else)` is the
generic fallback, not a model.

### Model name matching

Session files record decorated model names. AIProbe normalises them before looking
up a price: it lowercases, strips a vendor prefix (`anthropic/`,
`us.anthropic.`), strips a Bedrock version suffix (`:0`, `-v1`), and strips a
trailing eight-digit build date, so `claude-sonnet-4-5-20250929` finds
`claude-sonnet-4-5`.

It will also resolve a small allowlist of decorations — `-latest`, `-thinking`,
`-preview`, `-beta` — to the base model.

It will **not** resolve a version it has never seen. `claude-opus-4-7` does not
inherit the price of `claude-opus-4`, even though one is a prefix of the other,
because those are different generations at very different prices: opus-4 is $15/M
and the 4-5 generation is $5/M. Accepting that match reported roughly three times
the true cost of a real session, with nothing to indicate a guess had been made.

An unlisted version falls through to the generic rate below instead, and is named.

## The generic rate

A model with no rate of its own is priced at a fallback rate, currently $3/$15 per
million. `aiprobe pricing` shows it as `(anything else)`.

Every figure derived from it is labelled, names the model, and says how to fix it:

```
   Approximate: no rate on file for claude-opus-4-7, so a generic $3.00/M
   rate was used. Run `aiprobe pricing --init` and set the real rate to firm
   this up.
```

This is a change from the original design, which reported such a model as unknown on
the principle that a fabricated number is worse than nothing. That principle is
right about *unlabelled* numbers, and turned out to be wrong here: anyone on a model
newer than the built-in table saw no cost at all, which is not more honest, only less
useful. A rough figure you can see and correct beats a blank you cannot.

Set it under `default` in the price file:

```json
{ "default": { "input": 3, "output": 15 } }
```

Set it to zero to switch it off and get the explicit unknown back:

```json
{ "default": { "input": 0, "output": 0 } }
```

Note that the fallback never stands in for an exchange rate. A credit-billed runtime
still produces no dollar figure at all — see
[when the cost is unknown](#when-the-cost-is-unknown).

## Using your own rates

The effective rates are visible in two places: `aiprobe pricing` in a terminal, and
the **Prices** page in `aiprobe serve`. Both show all four rates per model, mark each
row as built-in or yours, and name the file to edit. The web page is read-only — the
server writes nothing but the analysis history — so editing happens in the file.

```console
$ aiprobe pricing --init
Wrote ~/.aiprobe/pricing.json
Edit the rates to match your invoice. Set "discount" to the fraction off list you
get, so 0.3 means you pay 70%.
```

`--init` writes the whole built-in table out, so changing a rate is a matter of
editing a number rather than knowing the schema. It refuses to overwrite an existing
file: running it twice must not destroy rates you typed in by hand.

```json
{
  "currency": "USD",
  "discount": 0,
  "default": { "input": 3, "output": 15 },
  "models": {
    "claude-sonnet-4-5": { "input": 3, "output": 15 },
    "our-self-hosted-llama": { "input": 0.05, "output": 0.05, "cache_read": 0 }
  },
  "note": "free text, ignored by the tool, for recording where these came from"
}
```

**Two numbers per model is all you need.** Rates are per **one million** tokens, and
the cache rates are derived from `input` unless you say otherwise:

| Field | Required | Default |
| --- | --- | --- |
| `input` | yes | — |
| `output` | yes | — |
| `cache_write` | no | `1.25 × input`, which is what Anthropic charges |
| `cache_read` | no | `0.1 × input`, which is what Anthropic and OpenAI both charge |

Set `cache_write` to `0` explicitly for a provider that does not bill for creating a
cache entry — OpenAI does not, and `--init` writes that zero out for the GPT models
for exactly this reason. An omitted field takes the derivation; an explicit `0` means
zero.

Anything in `models` wins over the built-in table, and models the built-in table has
never heard of are simply added. The rendered table marks each row `yours` or
`built-in` so you can see which is which.

Configuring all four rates for every model was the original design, and it made a
file nobody wants to open: eight numbers to change one, seven of which you would have
to look up.

A file that will not parse is reported and then ignored: you get a warning and the
built-in prices, not a failed analysis. Losing a whole report to a stray comma would
be a bad trade.

The file is written `0600`. It holds no secrets, but it is your configuration.

## Discounts

`discount` is the **fraction off list**, between 0 and 1. `0.3` means 30% off, so
you pay 70%.

This is ambiguous in English and worse in Chinese, where 折扣 conventionally names
the multiplier rather than the reduction (七折 = pay 70%). So every output states it
both ways:

```
Rates: built into AIProbe, list prices as of 2026-09, 30% off (paying 0.70x list).
```

A value of 1 or more is rejected rather than clamped, because the likely mistake is
passing `30` for 30%, and silently treating that as "everything is free" would be
worse than an error.

`--discount` on any analysis command overrides the file for that run. Because `0` is
a meaningful discount, the flag defaults to `-1`, meaning "not given, use the file".

## What "recoverable" is worth

The recoverable figure is the one place where the obvious calculation is wrong, so it
is worth being explicit.

Recoverable tokens are **not** valued at the uncached input rate. They are valued at
what the session actually paid per prompt token:

```
effective input rate = (input$ + cache write$ + cache read$) / prompt tokens × 1M
```

The reason is that most recoverable context is cache-eligible. MCP tool schemas sit
in the cached prefix; a file read three times is read from cache twice. The session
was paying a fraction of the input rate to carry them. Valuing that waste at the full
input rate produced, on the bundled demo session, a claimed saving of **$5.95 against
a $1.65 bill** — a figure that is not merely imprecise but self-evidently impossible,
and the fastest possible way to lose a reader's trust in every other number in the
report.

With the effective rate, the demo reports $0.95 recoverable against $1.65 spent: 57%
of the money, against 58% of the tokens. The two agree, which is the sanity check
that was failing before. A test asserts that recoverable can never exceed what was
spent.

Consequences worth knowing:

- The recoverable figure is **always an estimate**, marked with a leading `~`, even
  when the session's own cost is exact. The waste figures behind it are analysis, not
  billing data.
- It is a floor in one sense and an upper bound in another: the individual waste
  figures can overlap, so the total is an upper bound on any single fix; but figures
  that could not be measured are excluded, so it understates in the other direction.
  The report says both.
- Per-finding costs use the same rate, so they cannot add up to more than the
  session's cost.

## When the cost is unknown

There is no *measured* cost when the runtime reports no token counters, but there is
still a figure — see [when there are no counters](#when-there-are-no-counters). The
section is only ever blank when no model was recorded at all, because then there is no
rate to apply.

That last case is not hypothetical: **Cursor does not record which model answered**, so
a Cursor report shows no cost at all. Not a zero, and not a guess — there is no rate to
apply, and picking one would be inventing the answer. Every other section of a Cursor
report still works, because they describe the conversation rather than the bill.

`unknown_reason` records why nothing was measured even when a modelled figure is
present:

| Reason | Meaning | What to do |
| --- | --- | --- |
| `no_token_counters` | The runtime reports no token usage, so the counts and the cache split are derived. | Read the modelled figure; treat it as a floor. |
| `billed_in_credits` | The runtime bills in its own units, which are reported as themselves. | Read the credit figure and the modelled dollar figure. |
| `no_price_for_model` | Only when you have switched the generic rate off. The model is named. | Add its rate, or turn the fallback back on. |

## When there are no counters

Claude Code and Codex CLI report real token counters, so their cost is measured.
Kiro, Kiro CLI and Cursor do not. For those the cost is **modelled** from the
transcript rather than read off it:

```
   Session cost                          ~$546.76
   Recoverable                            ~$26.12    5%

   cache writes            1,737,898       $10.86
   cache reads         1,071,802,507      $535.90
```

The split comes from the context breakdown. Content enters the prompt once and is
re-sent on every later request, so the one-off inserted total is the cache-write side
and the remainder of the cumulative total is cache reads. On a real session that is
**99.8% reads**.

That ratio is the whole reason this is modelled rather than bounded. The first version
priced all cumulative context at the uncached input rate and reported **$5,367 for a
session that models out at $547** — a ten-fold overstatement, and the sort of number
that makes a reader distrust the rest of the report. The uncached reading is still
computed and reported, but as the upper bound it always was:

```
   Treat it as a floor. Cache entries expire, and a session with long gaps
   re-pays full price after each one, which this does not model. At the other
   extreme, nothing cached at all would have cost ~$5,367.70. An actively used
   session sits near the lower figure.
```

So the modelled figure is a floor and the ceiling is the roof, both are shown, and the
assumption between them is stated rather than hidden. What is *not* modelled:

- **Cache expiry.** A provider cache has a short time to live, so the first request
  after a long gap re-pays full price. This counts it as a cheap read, which is why the
  figure is a floor.
- **Completions.** A runtime that reports no input counters does not report output
  either, so output tokens are left at zero rather than invented.

The credit figure is reported alongside, untouched. The two are independent facts — one
is the runtime's own exact accounting, the other is derived tokens at a published rate —
and neither is converted into the other.

A session where some requests could be priced and others could not is reported as
`partial`, and its total is labelled a floor.

## Currencies

`currency` is a **label**. AIProbe performs no conversion, ever, because doing so
would need an exchange rate it has no honest way to obtain.

Setting it is useful when you enter rates in your own currency: the report then stops
claiming your euro figures are dollars. Recognised symbols are USD, EUR, GBP, JPY and
CNY; anything else is rendered as a code suffix (`12.34 SEK`) rather than dressed up
as dollars.

Across sessions, a mixed-currency set is **not** added up. The overview flags it and
counts the differently-denominated sessions as unpriced.

## Turning it off

`--no-cost` on any analysis command removes the cost section, the headline money
figures and the per-finding costs. The JSON omits `spend` entirely rather than
emitting zeroes, so a consumer can distinguish "pricing was off" from "this cost
nothing".

## Reading the JSON

```console
$ aiprobe pricing --json          # the effective price table
$ aiprobe report --json           # a report, including its spend section
```

The `spend` object carries the figures and their provenance:

| Field | Meaning |
| --- | --- |
| `known` | Whether a cost was established at all. When false, read `unknown_reason`. |
| `total` | The session cost, broken into the four classes plus a total. |
| `recoverable` | What the waste was worth. `estimated` is always true. |
| `prompt_tokens`, `prompt_cost` | The context side of the bill, excluding completions. |
| `effective_input_rate` | The blended rate described above, per million tokens. |
| `modelled` | The counts and cache split were derived, not measured. The figure is a floor. |
| `ceiling`, `ceiling_tokens` | The all-uncached upper bound, present when `modelled` is set. |
| `by_model` | One row per model charged, with its own usage, cost and four rates. |
| `unpriced_models` | Models used with no rate on file, only possible with the fallback off. |
| `approximated_models`, `approximate` | Models priced at the generic rate, and whether any part of the total was. |
| `fallback_rate` | The generic input rate, so the report can name what it approximated with. |
| `partial` | Some requests priced, some not: the total is a floor. |
| `discount`, `multiplier` | The same discount, both ways round. |
| `price_source` | `default`, `config` or `mixed`. |
| `prices_as_of` | The vintage of the built-in table. |
| `config_error` | Set when a price file existed and could not be read. |

Every `Cost` object carries `estimated`, and cost inherits the provenance of the
tokens it was computed from: estimated tokens produce an estimated cost, and the
renderers mark it the same way they mark an estimated token count.

## See also

- [architecture.md](architecture.md) — where pricing sits in the pipeline and why
  the rules engine knows nothing about money.
- [playbook.md](playbook.md) ([中文](playbook.zh-CN.md)) — what to actually change
  once you know what it costs.
- [SECURITY.md](../SECURITY.md) — every file AIProbe reads and writes, including
  `pricing.json`.
