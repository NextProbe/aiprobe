# Security Policy

*[中文版](SECURITY.zh-CN.md)*

## Reporting a vulnerability

Please report security issues privately, not as a public issue.

Use GitHub's [private vulnerability
reporting](https://github.com/NextProbe/aiprobe/security/advisories/new) on
this repository. It is the fastest route and keeps the report confidential until a
fix is available.

Please include:

- what the issue is and what an attacker could achieve,
- the version (`aiprobe --version`) and platform,
- a minimal way to reproduce it — see the note on session data below.

You can expect an acknowledgement within a few days and an assessment shortly
after. If a fix is needed you will be credited in the release notes unless you
would rather not be.

**Do not attach a real session transcript.** They contain your prompts, file
contents and command output. If reproduction needs session data, describe the
structure or construct a minimal synthetic file; `internal/demo/gen.go` shows how
the bundled synthetic session is built.

## Supported versions

AIProbe is pre-1.0. Fixes land on `main` and in the next release. There are no
maintained release branches yet.

## What counts as a vulnerability here

This tool reads your prompts and source code, so its threat model is unusual: the
sensitive asset is the *input*, and the risk is that input escaping the machine or
leaking into a file you then share. Anything that breaks one of these guarantees is
a security issue, even if nothing crashes:

| Guarantee | A violation would be |
| --- | --- |
| **No network.** The binary contains no HTTP client. | Any outbound connection, including a lookup, update check or CDN reference in generated HTML. |
| **Read only where your work lives.** AIProbe writes exactly two things: the report you asked for, at the path you gave, and its own state under `~/.aiprobe` — the analysis history, plus a price file if you ask for one. | Writing to, or modifying, a session transcript, an MCP config or agent configuration — or writing anything at all outside those two places. |
| **The stored history is a report.** Each file under `~/.aiprobe/analyses/` is byte-identical to `aiprobe --json`, so the two rows below cover it as well. | Anything in the store that would not be allowed in a report, or a store file left readable by other users (they are written `0600`). |
| **Loopback only.** `aiprobe serve` binds a loopback address and refuses anything else. | Any path that gets it listening on a routable interface, or serves a file outside the sessions it lists. |
| **No secrets in reports.** Excerpts pass a credential scrubber before reaching a report. | A token, key or password reaching a report file, terminal output, or the HTML page. |
| **No prompt content in files by default.** A report file carries counts, fingerprints, paths, tool names, session titles and short excerpts. A one-line, scrubbed preview of each question is shown in the terminal and on the local pages, and goes into a file only when it is written with `--prompt-preview`; the stored history never holds one. Session titles are the runtime's own, and some runtimes (Codex) title a session with the first line of its first message. | Prompt text, file contents or command output appearing in a file written without `--prompt-preview`; a question preview in the stored history or in a `serve` download; a preview that did not pass the credential scrubber. |
| **No telemetry.** Nothing is collected or reported. | Any collection, counting or phoning home. |

Several of these are enforced by tests rather than by review, so a regression
should fail the build:

- `TestNoForbiddenImports` fails if a network package is imported outside
  `internal/serve`.
- `TestNonLoopbackHostIsRefused` checks that `serve` refuses a non-loopback host
  with `ErrNotLoopback`, including an unresolvable hostname, which is treated as
  unsafe rather than assumed local.
- `TestServedPagesReferenceNoExternalOrigin` greps the bytes the server returns for
  any external origin, rather than trusting that none was added.
- `TestSecurityHeaders` checks the response headers on every route, including error
  pages.
- `TestAnalysesAreNotStoredDuringTests` and `TestDemoIsNotRemembered` keep the test
  suite and the bundled demo out of the history, so running the tests cannot change
  what the tool shows you.
- `TestMutatedDatabasesNeverPanic` covers the one binary format AIProbe parses.
  Cursor stores its chat history in a SQLite database, and AIProbe reads that file
  format directly rather than linking a driver, so a malformed or truncated database
  is untrusted input reaching a hand-written parser. Three thousand mutated
  databases, plus random bytes behind a valid signature, must all come back as
  errors: no panic, no unbounded loop, no read outside the buffer. A companion check
  confirms the sweep really reaches the b-tree and record decoding rather than being
  rejected at the header, so it cannot pass by being vacuous.

## Stale analyses

A stored analysis records the build that produced it, and the cache key includes that
version, so upgrading re-analyses rather than serving you an older build's conclusions.
`make build` gives every non-release build its own version stamp for this reason: two
builds that analyse differently must not share a version string. A `go install`ed
binary derives its version from Go's embedded VCS stamp.

If a report ever looks like it predates a change you know is in your binary,
`aiprobe history --clear` forces everything to be recomputed.

## The analysis history

AIProbe keeps the analyses it has run so a report opens instantly the second
time and so a trend is possible. This is the one thing it writes that you did not
explicitly ask for, so it is documented rather than incidental:

```
~/.aiprobe/
├── index.json            one record per analysis: identity, timing, headline figures, cost row
├── analyses/<id>.json     the full report, byte-identical to aiprobe --json
└── pricing.json           your token prices, only if you ran `aiprobe pricing --init`
└── paths.json             where to look for sessions, only if you ran `aiprobe runtimes --init`
```

- Each stored analysis **is** a report. It contains counts, fingerprints, file
  paths, tool names and short scrubbed excerpts, and nothing a report would not
  contain. No prompt text, no file contents, no command output.
- Question previews are never written here, whatever flags a command was given:
  the store uses the same JSON writer as a report file written without
  `--prompt-preview`, which drops them.
- Each record in `index.json` keeps the session's title as the runtime gave it,
  and a cost row copied from the analysis (cost, credits, token totals, question
  count, the costliest question's share). A runtime's title can quote the first
  message: Codex titles a session with the first line of its first message, so the
  `title` stored for a Codex session is exactly that line.
- Files are written `0600`, through a temporary file and a rename, so an
  interrupted run cannot leave a partial file behind.
- The history is capped at 500 analyses; the oldest are evicted with their files.
- `--no-store` on `analyze`, `report`, `overview` and `serve` records nothing.
  `aiprobe history --clear` deletes everything already recorded, files
  included. `aiprobe history --path` prints the directory.
- The bundled `demo` is never recorded.

Reading it needs no special tooling: it is JSON reports in a directory, so you can
inspect or delete it with a text editor.

## The local web interface

`aiprobe serve` binds loopback only, as the table above says. Two of its routes are
called back by the report page and get extra checks, because one returns question
text and the other starts work:

- `/report/previews` (the one-line previews of a session's questions) and
  `/report/reanalyse` (re-run an analysis that predates the questions panel) answer
  only a request whose `Host` is literally `localhost`, a loopback address or the
  `--host` the server was started with. There is no DNS lookup, so a page that
  rebinds its own domain to `127.0.0.1` cannot read them. Re-analysis is a POST, and
  it also refuses an `Origin` other than the server's own, `null` included.
- Previews live only in the server process's memory, in a small cache that a
  refresh empties. They are never written to the history or to a download.
- Loopback is reachable by every account on the machine, not only yours, and the
  previews route has no authentication. On a machine other people can log in to,
  any local account could read the scrubbed one-line previews of every session the
  server lists; run `aiprobe serve --no-prompt-preview` there, which turns the
  route off.

## The price file

`paths.json` changes **where AIProbe reads from**, which is worth stating plainly in
a document about a tool that reads your prompts. It cannot widen what is read: an
override replaces one runtime's session directory with one you named, and each
adapter still reads only session files under it. It cannot cause a write anywhere,
because nothing in the analysis path writes outside the two locations above. What it
does mean is that a wrong entry points the reader at the wrong sessions, which is why
`aiprobe runtimes` prints the origin of every path it used and why a file that cannot
be parsed is ignored with a warning rather than half-applied.

`pricing.json` is the one file AIProbe writes that is yours to edit rather than its
own bookkeeping. It exists only if you create it:

- It is written **only** by `aiprobe pricing --init`, never as a side effect of an
  analysis, and `--init` refuses to overwrite an existing file so a second run cannot
  destroy rates you typed in.
- It holds unit prices, a currency label and a discount. It is not a credential
  store and nothing in it is sent anywhere — there is nowhere for it to be sent.
- It is written `0600`, like the history, because it is your configuration even
  though it holds no secrets.
- A file that cannot be parsed produces a warning and is ignored. Built-in prices
  are used instead and the analysis still runs.
- Deleting it reverts to the built-in list prices. Nothing else depends on it.

Costs derived from it appear in reports, so a report you share carries whatever rates
you configured. If your negotiated pricing is confidential, treat a report as
carrying it — see the note on sharing reports below.

See [docs/pricing.md](docs/pricing.md) for the format and for what the numbers mean.

## Out of scope

- Reports you generate and then share yourself. A report contains file paths, tool
  names and short excerpts by design; treat it with the same care as a stack trace
  from your codebase.
- The absence of authentication on `aiprobe serve`. This is deliberate: the
  server binds loopback only, so the only client that can reach it is a process on
  your machine, which already has filesystem access to everything the server can
  read. A password there would add a step without adding a boundary. If you find a
  way to reach it from off-machine, *that* is very much in scope. On a machine
  shared with other accounts, see [the local web interface](#the-local-web-interface)
  for what they can reach and how to turn it off.
- Wrong numbers. An incorrect estimate is a bug, and worth reporting as one, but it
  is not a security issue. Use a normal issue.
