# AIProbe screenshots

The images in this directory are referenced from [../../README.md](../../README.md)
and its translations. All are generated from the bundled synthetic demo session
— nothing on your machine is required to reproduce them, and no real
transcript ever appears in a screenshot.

| File | What it shows |
| --- | --- |
| `terminal.png` | `aiprobe demo` running in a dark terminal |
| `dashboard-hero.png` | Score, waste summary and cost row of `aiprobe demo --html` (light theme, default) |
| `dashboard-hero-dark.png` | The same header rendered in the dark theme |
| `dashboard-findings.png` | The "What to change" panel |
| `dashboard-cache.png` | The "Prompt cache" panel with per-turn charts |
| `dashboard-mcp.png` | The "MCP tool tax" panel |
| `dashboard-dup.png` | The "Duplicate context" panel |
| `dashboard-score.png` | The "Context efficiency score" breakdown |
| `dashboard-questions.png` | The "Questions" panel, question 4 expanded to its requests (written with `--prompt-preview`) |
| `dashboard-questions-zh.png` | The same panel in 简体中文 |
| `serve-sessions.png` | `aiprobe serve` Sessions page (English), with each session's stored cost, credits and question count |
| `serve-sessions-zh.png` | `aiprobe serve` Sessions page (简体中文) |
| `serve-runtimes.png` | `aiprobe serve` Runtimes page |
| `serve-ranking.png` | The "Cost ranking" tab of the `aiprobe serve` aggregate: every session by what it cost |
| `serve-ranking-zh.png` | The same ranking in 简体中文 |

All dashboard images are rendered at width 1280 with headless Chrome. The
terminal image wraps `docs/demo-report.txt` in an inline HTML template.
The `serve-*` images start `aiprobe serve` in a sandbox — a scratch home
directory holding one fake project with the bundled demo session and two
shorter sessions cut from it at question boundaries, so the cost ranking has
something to rank — so the page never renders the developer's real
transcripts, project paths, or timestamps. The analyses that fill the Sessions
columns go to the sandbox's own store, which is deleted with the sandbox.

## Regenerating

```powershell
# Windows
powershell -ExecutionPolicy Bypass -File scripts/screenshots.ps1
```

```sh
# macOS / Linux
bash scripts/screenshots.sh
```

Both scripts:

1. Find a headless-capable Chrome or Chromium.
2. Build `bin/aiprobe` from the current tree.
3. Emit every PNG in this directory.

The scripts are not part of the build or the test suite. They exist so anyone
updating the README can regenerate the screenshots with one command.
