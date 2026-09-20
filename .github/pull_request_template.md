<!--
Thanks for the patch. Keep one concern per pull request: a refactor bundled with a
behaviour change is hard to review and harder to revert.
-->

## What this changes

<!-- And why. The diff already shows what; the reasoning is the part reviewers
     cannot reconstruct. -->

## How it was verified

<!-- `make check` passing is the baseline, not the answer. What did you actually
     observe? -->

- [ ] `make check` passes
- [ ] Added or updated a test that would have caught this

## Checklist

- [ ] Numbers keep their provenance: nothing measured is presented as estimated or
      the reverse, and no `unknown` was replaced with a zero or a plausible guess.
- [ ] No new dependency. If there is one, the description says why the standard
      library will not do.
- [ ] No new network access. `internal/serve` is the only package allowed to
      listen, and only on loopback.

<!-- Delete any that do not apply -->

- [ ] **Changed the scoring formula:** `docs/scoring.md` updated in the same commit.
- [ ] **Changed demo output:** `docs/demo-report.txt` regenerated and the headline
      blocks in `README.md` and `README.zh-CN.md` updated.
- [ ] **Added user-visible text:** it goes through `internal/i18n`, with entries in
      every locale, not a string literal in an analyzer.
- [ ] **Added an adapter:** it is the only code that knows the runtime exists; no
      analyzer special-cases it.
- [ ] **Added a terminal table column:** checked at `--width 60 76 100 120` in both
      `--lang en` and `--lang zh`.
