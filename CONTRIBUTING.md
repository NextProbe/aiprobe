# Contributing to AIProbe

*[中文版](CONTRIBUTING.zh-CN.md)*

Thanks for looking. Bug reports, adapters for other agents, and translations are
the three contributions that help most right now.

Before writing code, it is worth knowing that this project has a narrow scope on
purpose. A feature that does not help answer *where tokens went*, *why they were
wasted*, or *what to change* will probably be declined however well it is
implemented — that is not a reflection on the work, just on the boundary.

## Getting set up

```sh
git clone https://github.com/NextProbe/aiprobe
cd aiprobe
make check      # gofmt check, go vet, full test suite
make build      # ./bin/aiprobe
./bin/aiprobe demo
```

Go 1.25 or newer. The only dependency is [cobra](https://github.com/spf13/cobra);
tests use the standard library only. Please keep it that way — a diagnostic tool
people run against their own prompts should be auditable in an afternoon.

`make check` is what CI runs. If it passes locally it should pass there.

## The rules that are not style preferences

Four properties are enforced by tests rather than review, because they are the
reason anyone would trust the output. If your change trips one of these, the fix
is almost never to change the test.

**1. No network client in the binary.** `TestNoForbiddenImports` walks the import
graph and fails if `net`, `net/http` or friends appear outside a short list of
allowed exceptions. The README promises there is no HTTP client; the test is what
makes that a fact rather than a claim. `internal/serve` is on the exception list
and is the only place allowed to listen — on loopback only.

**2. Measured, estimated, inferred and unknown stay distinct.** Never widen a
`trace.OptInt` into a plain `int` with a zero default, and never rescale an
estimate so a total looks exact. If attribution is impossible, the answer is
`unknown` and it must survive all the way into the JSON.

**3. No model in the analysis path.** Scores and recommendations are deterministic
formulas and rules. `TestDeterministic` and `TestDocMatchesImplementation` keep the
score reproducible and keep [docs/scoring.md](docs/scoring.md) matching the code.
If you change a weight or a target, update the document in the same commit — the
build fails otherwise, by design.

**4. Documentation that quotes output stays true.** `TestReadmeHeadlineMatchesDemo`,
`TestTranslatedReadmesStayInStep` and `TestCommittedDemoReportIsCurrent` compare
the READMEs and `docs/demo-report.txt` against what the binary actually prints. A
README with stale numbers is worse than one with none.

If your change alters the demo output, regenerate the committed copy:

```sh
make build
{ echo '$ aiprobe demo'; echo; ./bin/aiprobe demo --no-color; } > docs/demo-report.txt
```

and update the headline blocks in `README.md` and `README.zh-CN.md` from
`./bin/aiprobe demo --no-color` and `--no-color --lang zh`.

## Adding an adapter

This is the most useful thing you can add. An adapter turns one runtime's session
files into `internal/trace`, and it is the *only* place allowed to know that runtime
exists.

There is no interface to implement. An adapter is a package exposing two functions,
wired into `internal/adapters`:

```go
func ParseFile(path string) (*trace.Trace, error)  // parse.go
func Detect(path string) bool                      // discover.go
```

`Detect` has to be probed **before** Claude Code in `adapters.Detect`, because
Claude Code is the fallback: an adapter that forgets is silently parsed as Claude
Code and produces a plausible, wrong report.

A session does not have to be one file. Kiro CLI reads a transcript plus a metrics
sidecar; Cursor reads one conversation out of a SQLite database shared by all of
them, addressed as `path#<id>`. If your runtime is like the latter, `Discover` is
what enumerates the conversations, and a bare path must pick one and say so rather
than blending several.

Parsing the JSON is the easy part. **Five questions decide whether the resulting
report is honest**, and they are worth answering before writing any code:

1. **Does the runtime record provider token usage, per request?** If yes, those
   counts are ground truth: set `Request.Usage` and everything downstream can be
   exact. If no, every figure becomes an estimate — which is supported, but see (4).
2. **Does it record prompt cache counters?** If not, leave the cache fields at zero
   *and* make sure the cache analyzer reports `unknown` rather than a 0% hit ratio.
   "No cache data" and "your cache never hits" look nothing alike to a reader.
3. **Does anything leave the context after entering it once?** The question most
   often answered wrongly and the most expensive to get wrong. Kiro drops file-write
   bodies after the prompt that produced them. Content that stays is charged to every
   later request; content that is dropped is charged once. Confusing the two
   overstated a real session by roughly 3x during development. Mark those events
   `Event.Transient = true`.
4. **Does the runtime publish anything your estimate can be checked against?** A
   context-window percentage, a message count, a byte total — anything that moves with
   context size. If it does, use it: with no token counters that check is the
   difference between an estimate and a guess, and it must be independent of the
   assumed window size.

   If it genuinely publishes nothing, the adapter is still worth having, but it has to
   change what the report claims rather than quietly produce the same-looking numbers.
   Cursor is the worked example: no tokens, no cache, no cost, no window, no model. Its
   adapter leads with a warning saying every figure is an estimate with nothing behind
   it, reports no cost at all rather than a priced guess, and leaves the content
   findings — repeated context, files read twice, oversized tool results — to carry the
   report, because those are true of the record regardless of what was billed.

   The line to hold is that "unverifiable" has to be visible to the reader, not merely
   recorded in the capability table.
5. **Which messages did a person write?** The questions section splits a session at
   every message a person wrote, and only the adapter can tell those apart from what
   the runtime, a hook or a workflow put into the conversation. Once you classify,
   set `Trace.Prompts = trace.PromptsClassified` and give every user-role event an
   `Origin`: `prompt`, `command` or `shell` for what a person typed (these open a
   question), `preamble` for the wrapper the runtime writes in front of every prompt,
   `injected` for everything else. Put the text a person would recognise in
   `Display` (a slash command with its arguments, never command output), record
   `Signal` on interruptions and failed turns, and add `Turns` when the runtime
   reports its own turns or per-turn charges, with `Start = -1` only when a turn's
   counts really sit on a request chosen by position. Set `UnreportedUsage`,
   `UnreadableUserRecords` and `CreditsUnavailable` (with its reason id) where they
   apply. Read new transcript fields as `json.RawMessage` and decode them in the
   branch that needs them, so a field of an unexpected shape counts as missing
   rather than costing the whole line. An adapter that classifies nothing still
   works: the report shows the session as one bucket and says the split is
   unavailable.

Before opening the pull request:

- [ ] `grep -ri yourruntime internal/analyzer internal/report` is empty. No analyzer or
      renderer may mention a runtime by name.
- [ ] `Capabilities` describes what the format really provides, including the `false`
      entries.
- [ ] Detection is structural, reads only a few lines, and does not misfire on the
      other runtimes' fixtures.
- [ ] Undecodable lines are counted and skipped, never fatal.
- [ ] Missing quantities are `OptInt`/`OptFloat` unknowns, not zeros, all the way into
      the JSON.
- [ ] Every user-role event has an `Origin`, and `Trace.Prompts` says whether they
      were classified.
- [ ] Tests cover a normal session, a truncated file, an empty file, and a session with
      no tool calls.
- [ ] **Fixtures are synthetic.** Never commit a real transcript: they contain prompts
      and source code.
- [ ] Runtime added to the `--adapter` help and the README table of session locations.

If you find yourself wanting to special-case your runtime inside an analyzer, the trace
model is missing a concept — raise that as the issue instead of adding the branch. That
is exactly what happened with Kiro: the analyzers needed no changes, but the model had
no way to express "this content left the context". A model that cannot say that is not
provider-neutral, it is just Claude-shaped.

## Adding or fixing a translation

Message catalogs live in `internal/i18n/catalog_<locale>.go`. Adding a locale is
adding one file plus one line in `Locales()`; a set of tests then holds it to the
same standard as the others:

- `TestEveryLocaleCoversEnglish` — every id English defines must exist.
- `TestNoLocaleInventsIDs` — no ids that English does not have.
- `TestPlaceholdersMatch` — the same `{named}` placeholders on both sides.
- `TestLiteralMessageIDsExist` — every id the code or the HTML template looks up
  actually exists, in every locale. Without this, a typo ships as body text.

Two things to know before translating:

- **Placeholders are named, not positional** (`{tokens}`, not `%s`). Chinese and
  English order clauses differently, so a translator has to be able to move a value
  to where the sentence needs it.
- **Analyzers do not produce prose.** They record a message id plus arguments, and
  the reporter renders it. So a new analyzer sentence means a new catalog entry,
  not a new string literal in the analyzer. The English rendering the JSON report
  carries comes from the same catalog entry, which is why the two cannot drift.

Adding a locale is one new `catalog_<locale>.go` plus one line in `Locales()`. Two
things are deliberately *not* translated: model and tool names, which must match what
you would type, and the `schema` field in the JSON report.

For terminal output, remember CJK glyphs are double width and that the layout is
aligned to a hard column limit. The renderer handles the wrapping, but if you add a
table column, check it at several widths:

```sh
for w in 60 76 100 120; do ./bin/aiprobe demo --no-color --lang zh --width $w; done
```

Documentation is bilingual: an English file and a `.zh-CN.md` beside it, each linking
to the other at the top. If you change one, change both — a translation that quietly
falls behind is worse than an untranslated page, because nobody can tell.

## Commits and pull requests

- One concern per pull request. A refactor bundled with a behaviour change is hard
  to review and harder to revert.
- Explain *why* in the commit message. The diff already shows what.
- Include a test for a bug fix, so the bug cannot return quietly.
- Comments should explain reasoning, not restate the code. This codebase leans on
  comments for the non-obvious tradeoffs; please match that.

## Reporting bugs

The most useful report is a session that produces a wrong number, plus what you
expected and why. Session transcripts contain your prompts and file contents, so
**do not attach one**. Instead:

- `aiprobe analyze <session> --json` and attach the parts that are wrong, or
- describe the shape: how many requests, which runtime and version, whether MCP
  servers were involved, roughly where the number went astray.

If the parser skipped lines, the report says so on the `parsing` line — include
that count.

## Reporting a vulnerability

Please do not open a public issue. See [SECURITY.md](SECURITY.md).

## Licence

Contributions are accepted under the [MIT Licence](LICENSE), the same terms as the
project.
