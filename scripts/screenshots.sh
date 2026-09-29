#!/usr/bin/env bash
# Screenshot generator (macOS / Linux).
#
# POSIX sibling of scripts/screenshots.ps1. Same inputs (bundled synthetic
# demo session), same three outputs under docs/screenshots/. Nothing in this
# script touches real user data, so it is safe to run and commit from any
# machine.
#
# Usage:
#   bash scripts/screenshots.sh
#
# Requires: Go 1.25+ (to rebuild bin/aiprobe if missing) and Google Chrome or
# Chromium on PATH (or in one of the standard install locations below).

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
cd "$repo_root"

out_dir="$repo_root/docs/screenshots"
mkdir -p "$out_dir"

# Locate a headless-capable browser.
find_browser() {
  local candidates=(
    google-chrome
    google-chrome-stable
    chromium
    chromium-browser
    "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
    "/Applications/Chromium.app/Contents/MacOS/Chromium"
    "/Applications/Google Chrome Canary.app/Contents/MacOS/Google Chrome Canary"
  )
  for c in "${candidates[@]}"; do
    if command -v "$c" >/dev/null 2>&1; then
      command -v "$c"
      return
    fi
    if [ -x "$c" ]; then
      echo "$c"
      return
    fi
  done
  echo "No Chrome or Chromium found. Install one, or edit this script." >&2
  exit 1
}
browser=$(find_browser)
echo "Using browser: $browser"

# Build a fresh binary if needed.
binary="$repo_root/bin/aiprobe"
if [ ! -x "$binary" ]; then
  echo "Building bin/aiprobe..."
  mkdir -p "$repo_root/bin"
  go build -o "$binary" .
fi

work=$(mktemp -d -t aiprobe-screenshots.XXXXXX)
trap 'rm -rf "$work"' EXIT

# Chrome writes the screenshot asynchronously on some systems, so retry with a
# short delay if the file has not appeared yet.
shoot() {
  local out="$1" url="$2" w="$3" h="$4" budget="${5:-800}"
  local i
  for i in 1 2 3; do
    rm -f "$out"
    "$browser" \
      --headless \
      --disable-gpu \
      --hide-scrollbars \
      --no-sandbox \
      --window-size="$w,$h" \
      --virtual-time-budget="$budget" \
      --screenshot="$out" \
      "$url" \
      >/dev/null 2>&1 || true
    sleep 0.2
    if [ -s "$out" ]; then return; fi
    sleep 1
  done
  echo "Chrome did not write $out after 3 attempts" >&2
  exit 1
}

url_of() { printf 'file://%s' "$1"; }

# ------------------------------------------------------------------ #
# 1) terminal.png                                                    #
# ------------------------------------------------------------------ #
# Wrap the committed demo-report.txt in a dark-terminal HTML page. The font
# stack starts with DejaVu Sans Mono / Liberation Mono because those are what
# most Linux distributions install; macOS falls through to Menlo, which
# covers the box-drawing glyphs the report uses.
terminal_src="$work/terminal.html"
terminal_out="$out_dir/terminal.png"

python3 - "$repo_root/docs/demo-report.txt" "$terminal_src" <<'PY'
import html, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src, "r", encoding="utf-8") as f:
    body = f.read()
open(dst, "w", encoding="utf-8").write(
    "﻿"
    "<!DOCTYPE html>\n"
    "<html><head><meta charset=\"utf-8\"><title>aiprobe demo</title>\n"
    "<style>\n"
    "html,body{margin:0;padding:0;background:#0b0d10}\n"
    "body{padding:28px 32px}\n"
    "pre{color:#d7dae0;margin:0;white-space:pre;font-size:14px;line-height:1.45;"
    "font-family:'DejaVu Sans Mono','Liberation Mono','Noto Sans Mono','Ubuntu Mono',"
    "Menlo,Consolas,'Courier New',monospace}\n"
    "</style></head><body><pre>" + html.escape(body) + "</pre></body></html>\n"
)
PY

echo "Rendering terminal.png..."
shoot "$terminal_out" "$(url_of "$terminal_src")" 1200 2400

# ------------------------------------------------------------------ #
# 2) dashboard-hero.png                                              #
# 3) dashboard-findings.png                                          #
# 4) dashboard-cache.png / dashboard-mcp.png / dashboard-dup.png /   #
#    dashboard-score.png                                             #
# 5) dashboard-hero-dark.png                                         #
# ------------------------------------------------------------------ #
# `aiprobe demo --html` emits the interactive dashboard. Chrome headless
# renders as the `print` media type, which the report's own stylesheet uses
# to hide the tab bar and show every panel; injecting a small <style> block
# undoes that. Each panel variant runs a tiny script that clicks the right
# tab and pins the scroll position to the top; the dark hero variant
# forces data-theme="dark" so the reader sees both themes side by side.
dash_html="$work/dashboard.html"
"$binary" demo --html -o "$dash_html"

python3 - "$dash_html" "$work" <<'PY'
import sys
src, work = sys.argv[1], sys.argv[2]
raw = open(src, "r", encoding="utf-8").read()

shared_style = (
    "<style>\n"
    "@media print {\n"
    "  .tabs, .copy-row { display: flex !important; }\n"
    "  .panel { display: none !important; }\n"
    "  .panel.active { display: block !important; }\n"
    "  body { background: var(--bg) !important; color: var(--fg) !important; }\n"
    "}\n"
    "</style>\n"
)

bom = "﻿"

def with_style(raw):
    return raw.replace("</head>", shared_style + "</head>")

def with_tab(raw, tab_id):
    script = (
        "<script>\n"
        "(function () {\n"
        "  function activate() {\n"
        "    var tab = document.querySelector('.tab[data-target=\"" + tab_id + "\"]');\n"
        "    if (tab) { tab.click(); window.scrollTo(0, 0); }\n"
        "  }\n"
        "  if (document.readyState === 'loading') {\n"
        "    document.addEventListener('DOMContentLoaded', function () { setTimeout(activate, 50); });\n"
        "  } else { setTimeout(activate, 50); }\n"
        "})();\n"
        "</script>\n"
    )
    return raw.replace("</body>", script + "</body>")

hero = with_style(raw)
open(work + "/dashboard-hero.html", "w", encoding="utf-8").write(bom + hero)

for tab in ("panel-findings", "panel-cache", "panel-mcp", "panel-dup", "panel-score"):
    stem = "dashboard-" + tab.replace("panel-", "") + ".html"
    name_map = {"dup": "dup", "findings": "findings", "cache": "cache", "mcp": "mcp", "score": "score"}
    open(work + "/" + stem, "w", encoding="utf-8").write(bom + with_tab(hero, tab))

# Dark hero: same content but with data-theme="dark" on <html>.
import re
dark = re.sub(r"<html([^>]*)>", r'<html\1 data-theme="dark">', hero, count=1)
open(work + "/dashboard-hero-dark.html", "w", encoding="utf-8").write(bom + dark)
PY

echo "Rendering dashboard-hero.png..."
shoot "$out_dir/dashboard-hero.png" "$(url_of "$work/dashboard-hero.html")" 1280 900

echo "Rendering dashboard-findings.png..."
shoot "$out_dir/dashboard-findings.png" "$(url_of "$work/dashboard-findings.html")" 1280 1600 1500

echo "Rendering dashboard-cache.png..."
shoot "$out_dir/dashboard-cache.png" "$(url_of "$work/dashboard-cache.html")" 1280 1400 1500

echo "Rendering dashboard-mcp.png..."
shoot "$out_dir/dashboard-mcp.png" "$(url_of "$work/dashboard-mcp.html")" 1280 1400 1500

echo "Rendering dashboard-dup.png..."
shoot "$out_dir/dashboard-dup.png" "$(url_of "$work/dashboard-dup.html")" 1280 1400 1500

echo "Rendering dashboard-score.png..."
shoot "$out_dir/dashboard-score.png" "$(url_of "$work/dashboard-score.html")" 1280 1200 1500

echo "Rendering dashboard-hero-dark.png..."
shoot "$out_dir/dashboard-hero-dark.png" "$(url_of "$work/dashboard-hero-dark.html")" 1280 900

# Sweep the older experiment file.
rm -f "$out_dir/dashboard-hero-light.png"

# ------------------------------------------------------------------ #
# 6) serve-sessions.png (the local web interface)                    #
# ------------------------------------------------------------------ #
# `aiprobe serve` renders real sessions, so screenshotting it as-is
# would leak private information. We sandbox the server: fresh env
# vars redirect $HOME and every runtime to a scratch directory that
# holds one fake Claude Code project containing the bundled demo
# session. The result is a serve page whose only content is the same
# synthetic session everything else uses.
#
# The sandbox lives at /tmp/aiprobe-screenshots (not inside $HOME) so
# the Runtimes page's rendered directory does not leak the developer's
# username.
sandbox_root=/tmp/aiprobe-screenshots
rm -rf "$sandbox_root"
sandbox="$sandbox_root/home"
fake_project="$sandbox/.claude/projects/-Users-dev-code-orders-api"
mkdir -p "$fake_project"
cp "$repo_root/internal/demo/testdata/demo-session.jsonl" \
   "$fake_project/9f3c71d2-4e8a-4c15-b7a0-2d6ef1a90b34.jsonl"

port=17777
(
    HOME="$sandbox" \
    CLAUDE_CONFIG_DIR="$sandbox/.claude" \
    KIRO_CONFIG_DIR="$sandbox/.kiro" \
    CURSOR_CONFIG_DIR="$sandbox/.cursor" \
    CODEX_HOME="$sandbox/.codex" \
    "$binary" serve --port "$port" --quiet --no-store &
    echo $! > "$work/serve.pid"
    wait
) &
serve_wrap_pid=$!

# Kill the sandbox server and clean the scratch dir on exit.
cleanup_serve() {
    if [ -f "$work/serve.pid" ]; then
        kill "$(cat "$work/serve.pid")" 2>/dev/null || true
    fi
    rm -rf "$sandbox_root"
}
trap 'cleanup_serve; rm -rf "$work"' EXIT

# Wait for the server to accept requests.
ready=0
for _ in $(seq 1 40); do
    if curl -sSf -o /dev/null "http://127.0.0.1:$port/healthz" 2>/dev/null; then
        ready=1; break
    fi
    sleep 0.2
done
if [ "$ready" != 1 ]; then
    echo "aiprobe serve did not respond on port $port" >&2
    exit 1
fi

echo "Rendering serve-sessions.png..."
shoot "$out_dir/serve-sessions.png" "http://127.0.0.1:$port/?lang=en" 1280 900

echo "Rendering serve-runtimes.png..."
shoot "$out_dir/serve-runtimes.png" "http://127.0.0.1:$port/runtimes?lang=en" 1280 900

echo "Rendering serve-sessions-zh.png..."
shoot "$out_dir/serve-sessions-zh.png" "http://127.0.0.1:$port/?lang=zh" 1280 900

echo
echo "Done. Screenshots:"
ls -la "$out_dir"/*.png
