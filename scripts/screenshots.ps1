# Screenshot generator (Windows / PowerShell).
#
# Regenerates the images referenced from README.md. All input is the bundled
# synthetic session emitted by `aiprobe demo` -- no real transcript is ever
# touched, so this script is safe to run and commit from any machine.
#
# Usage:
#   powershell -ExecutionPolicy Bypass -File scripts/screenshots.ps1
#
# Outputs, all under docs/screenshots/:
#   terminal.png            colour-fidelity of `aiprobe demo` in a terminal
#   dashboard-hero.png      first viewport of `aiprobe demo --html`
#   dashboard-findings.png  the "What to change" section of the same report

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo

$outDir = Join-Path $repo 'docs/screenshots'
New-Item -ItemType Directory -Force -Path $outDir | Out-Null

# Locate a headless-capable browser. Chrome is preferred; Edge is the fallback
# because it ships with Windows and works with the same --headless flags.
function Get-Browser {
    $candidates = @(
        'C:\Program Files\Google\Chrome\Application\chrome.exe',
        'C:\Program Files (x86)\Google\Chrome\Application\chrome.exe',
        'C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe',
        'C:\Program Files\Microsoft\Edge\Application\msedge.exe'
    )
    foreach ($p in $candidates) {
        if (Test-Path $p) { return $p }
    }
    throw 'No Chrome or Edge found. Install Google Chrome, or edit this script.'
}
$browser = Get-Browser
Write-Host "Using browser: $browser"

# Chrome on Windows can return control before it has finished writing the
# screenshot, so every invocation goes through Start-Process -Wait to force a
# synchronous exit. A retry (with a small delay) protects against flaky first
# starts on a cold profile.
function Invoke-Chrome([string]$out, [string]$url, [int]$w, [int]$h, [int]$budgetMs = 800) {
    $args = @(
        '--headless',
        '--disable-gpu',
        '--hide-scrollbars',
        '--no-sandbox',
        "--window-size=$w,$h",
        "--virtual-time-budget=$budgetMs",
        "--screenshot=$out",
        $url
    )
    for ($i = 0; $i -lt 3; $i++) {
        if (Test-Path $out) { Remove-Item -Force $out }
        Start-Process -FilePath $browser -ArgumentList $args -NoNewWindow -Wait | Out-Null
        Start-Sleep -Milliseconds 200
        if ((Test-Path $out) -and (Get-Item $out).Length -gt 0) { return }
        Start-Sleep -Seconds 1
    }
    throw "Chrome did not write $out after 3 attempts"
}

# Build a fresh binary so the report reflects the current tree.
$binary = Join-Path $repo 'bin/aiprobe.exe'
if (-not (Test-Path $binary)) {
    Write-Host 'Building bin/aiprobe.exe...'
    New-Item -ItemType Directory -Force -Path (Join-Path $repo 'bin') | Out-Null
    & go build -o $binary .
    if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
}

$work = Join-Path $env:TEMP ("aiprobe-screenshots-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Force -Path $work | Out-Null
try {
    # ------------------------------------------------------------------ #
    # 1) terminal.png                                                    #
    # ------------------------------------------------------------------ #
    # Wrap the committed demo-report.txt in a page that mimics a dark
    # terminal. We render the whole report (not the first N lines) so the
    # image captures the report's shape end to end -- the reader gets to see
    # that there really are seven sections, not just a headline.
    # Explicit -Encoding UTF8: Windows PowerShell 5.1 (what `powershell.exe`
    # launches by default) reads files as the OEM code page unless told
    # otherwise, and on a zh-CN Windows that turns the box-drawing bytes into
    # mojibake before they ever reach the browser. PowerShell 7 already
    # defaults to UTF-8, so the flag is harmless there.
    $reportTxt = Get-Content -Raw -Encoding UTF8 -Path (Join-Path $repo 'docs/demo-report.txt')
    # Manual escape -- HttpUtility isn't loaded by default in PowerShell 7, and
    # the three characters are all a <pre> block needs.
    $encoded = $reportTxt -replace '&','&amp;' -replace '<','&lt;' -replace '>','&gt;'
    $terminalHtml = @"
<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>aiprobe demo</title>
<style>
  html, body { margin: 0; padding: 0; background: #0b0d10; }
  body { padding: 28px 32px; }
  pre {
    color: #d7dae0;
    /* Consolas covers the box-drawing and block characters the report uses
       and ships with Windows -- the "Cascadia Mono" chain we tried first
       renders them as tofu because Chrome only falls through the CSS chain
       for missing glyphs, not for missing glyph coverage. DejaVu / Liberation
       are the Linux fallbacks used by the POSIX sibling script. */
    font-family: Consolas, "DejaVu Sans Mono", "Liberation Mono",
                 "Noto Sans Mono", "Ubuntu Mono", "Courier New", monospace;
    font-size: 14px;
    line-height: 1.45;
    white-space: pre;
    margin: 0;
  }
</style></head>
<body><pre>$encoded</pre></body></html>
"@
    $terminalSrc = Join-Path $work 'terminal.html'
    Set-Content -Path $terminalSrc -Value $terminalHtml -Encoding UTF8

    $terminalOut = Join-Path $outDir 'terminal.png'
    Write-Host 'Rendering terminal.png...'
    Invoke-Chrome $terminalOut ("file:///" + ($terminalSrc -replace '\\','/')) 1200 2400

    # ------------------------------------------------------------------ #
    # 2) dashboard-hero.png                                              #
    # ------------------------------------------------------------------ #
    $dashHtml = Join-Path $work 'dashboard.html'
    & $binary demo --html -o $dashHtml
    if ($LASTEXITCODE -ne 0) { throw 'aiprobe demo --html failed' }

    # ------------------------------------------------------------------ #
    # 2) dashboard-hero.png                                              #
    # ------------------------------------------------------------------ #
    # Chrome's --headless renders as `print` media, which the report's own
    # stylesheet uses to reveal every panel and hide the tab bar. That is
    # correct behaviour for real printing but wrong for a screenshot of the
    # web view. Two small tweaks fix it without touching the CLI: inject a
    # <style> block that neutralises the @media print rules, and inject a
    # script that clicks the tab we want. We also inject the CSS/JS BEFORE
    # </head> so it beats DOMContentLoaded races.
    $dashRaw = [System.IO.File]::ReadAllText($dashHtml, [System.Text.Encoding]::UTF8)

    $sharedStyle = @'
<style>
  /* Undo the print-media rules so headless Chrome sees the interactive view. */
  @media print {
    .tabs, .copy-row { display: flex !important; }
    .panel { display: none !important; }
    .panel.active { display: block !important; }
    body { background: var(--bg) !important; color: var(--fg) !important; }
  }
</style>
'@
    $heroInject = $sharedStyle
    $heroHtml = $dashRaw.Replace('</head>', $heroInject + '</head>')
    $heroFile = Join-Path $work 'dashboard-hero.html'
    $utf8bom = New-Object System.Text.UTF8Encoding($true)
    [System.IO.File]::WriteAllText($heroFile, $heroHtml, $utf8bom)

    $heroOut = Join-Path $outDir 'dashboard-hero.png'
    Write-Host 'Rendering dashboard-hero.png...'
    Invoke-Chrome $heroOut ("file:///" + ($heroFile -replace '\\','/')) 1280 900

    # ------------------------------------------------------------------ #
    # 3) dashboard-findings.png                                          #
    # ------------------------------------------------------------------ #
    # Same treatment as the hero, plus a script that activates the
    # findings tab and pins the scroll position to the top so the masthead
    # stays in frame. The script goes at the end of <body> so it runs
    # after the report's own tab controller has finished wiring up.
    $findingsHtml = $dashRaw.Replace('</head>', $sharedStyle + '</head>')
    $findingsScript = @'
<script>
(function () {
  function activate() {
    var tab = document.querySelector('.tab[data-target="panel-findings"]');
    if (tab) {
      tab.click();
      window.scrollTo(0, 0);
    }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { setTimeout(activate, 50); });
  } else {
    setTimeout(activate, 50);
  }
})();
</script>
'@
    $findingsHtml = $findingsHtml.Replace('</body>', $findingsScript + '</body>')
    $dashFindingsFile = Join-Path $work 'dashboard-findings.html'
    [System.IO.File]::WriteAllText($dashFindingsFile, $findingsHtml, $utf8bom)

    $findingsOut = Join-Path $outDir 'dashboard-findings.png'
    Write-Host 'Rendering dashboard-findings.png...'
    Invoke-Chrome $findingsOut ("file:///" + ($dashFindingsFile -replace '\\','/')) 1280 1600 1500

    # ------------------------------------------------------------------ #
    # 4) dashboard-<tab>.png for cache / mcp / dup / score               #
    # ------------------------------------------------------------------ #
    # Same recipe as findings, parameterised by tab id. Kept inline
    # rather than looped so each output file's name is grep-able and
    # each viewport height can be tuned per panel.
    function New-TabPage([string]$tabId) {
        $script = @"
<script>
(function () {
  function activate() {
    var tab = document.querySelector('.tab[data-target="$tabId"]');
    if (tab) { tab.click(); window.scrollTo(0, 0); }
  }
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { setTimeout(activate, 50); });
  } else { setTimeout(activate, 50); }
})();
</script>
"@
        $html = $dashRaw.Replace('</head>', $sharedStyle + '</head>').Replace('</body>', $script + '</body>')
        $file = Join-Path $work ("dashboard-" + $tabId + '.html')
        [System.IO.File]::WriteAllText($file, $html, $utf8bom)
        return $file
    }

    $panels = @(
        @{ id = 'panel-cache';  name = 'dashboard-cache.png';  h = 1400 },
        @{ id = 'panel-mcp';    name = 'dashboard-mcp.png';    h = 1400 },
        @{ id = 'panel-dup';    name = 'dashboard-dup.png';    h = 1400 },
        @{ id = 'panel-score';  name = 'dashboard-score.png';  h = 1200 }
    )
    foreach ($p in $panels) {
        $out = Join-Path $outDir $p.name
        Write-Host "Rendering $($p.name)..."
        $srcFile = New-TabPage $p.id
        Invoke-Chrome $out ("file:///" + ($srcFile -replace '\\','/')) 1280 $p.h 1500
    }

    # ------------------------------------------------------------------ #
    # 5) dashboard-hero-dark.png                                         #
    # ------------------------------------------------------------------ #
    # The dashboard has a full dark theme; the default hero screenshot
    # renders in the light theme because Chrome headless reports light as
    # the OS preference. Force data-theme="dark" so the reader sees both.
    $darkHtml = $dashRaw.Replace('</head>', $sharedStyle + '</head>')
    $darkHtml = $darkHtml -replace '<html([^>]*)>', '<html$1 data-theme="dark">'
    $darkFile = Join-Path $work 'dashboard-hero-dark.html'
    [System.IO.File]::WriteAllText($darkFile, $darkHtml, $utf8bom)

    $darkOut = Join-Path $outDir 'dashboard-hero-dark.png'
    Write-Host 'Rendering dashboard-hero-dark.png...'
    Invoke-Chrome $darkOut ("file:///" + ($darkFile -replace '\\','/')) 1280 900

    # Clean up the previous experiment: an identical "hero-light" file
    # confused earlier readers, since the default hero is already light.
    $legacyLight = Join-Path $outDir 'dashboard-hero-light.png'
    if (Test-Path $legacyLight) { Remove-Item -Force $legacyLight }

    # ------------------------------------------------------------------ #
    # 6) serve-sessions.png (the local web interface)                    #
    # ------------------------------------------------------------------ #
    # `aiprobe serve` renders the reader's real sessions -- with real
    # project paths, real session ids, real timestamps -- so screenshotting
    # it as-is would leak private information every time. Instead we start
    # the server in a sandbox: fresh env vars redirect every runtime and
    # $HOME to a scratch directory that contains one fake project holding
    # the bundled demo session. The result is a serve page whose only
    # content is the same synthetic data all the other screenshots use.
    #
    # The sandbox lives at a generic top-level path (never under the
    # developer's TEMP) because the Runtimes page prints the resolved
    # directory verbatim, and a path like C:\Users\<name>\AppData\Local\...
    # would leak the developer's username.
    $sandboxRoot = 'C:\aiprobe-screenshots'
    if (Test-Path $sandboxRoot) { Remove-Item -Recurse -Force $sandboxRoot }
    $sandbox = Join-Path $sandboxRoot 'home'
    $fakeProject = Join-Path $sandbox '.claude\projects\-Users-dev-code-orders-api'
    New-Item -ItemType Directory -Force -Path $fakeProject | Out-Null
    Copy-Item (Join-Path $repo 'internal/demo/testdata/demo-session.jsonl') `
        (Join-Path $fakeProject '9f3c71d2-4e8a-4c15-b7a0-2d6ef1a90b34.jsonl')

    $port = 17777
    $sandboxEnv = @{
        USERPROFILE       = $sandbox
        HOME              = $sandbox   # POSIX fallback, harmless on Windows
        CLAUDE_CONFIG_DIR = Join-Path $sandbox '.claude'
        KIRO_CONFIG_DIR   = Join-Path $sandbox '.kiro'
        CURSOR_CONFIG_DIR = Join-Path $sandbox '.cursor'
        CODEX_HOME        = Join-Path $sandbox '.codex'
    }

    # Start `aiprobe serve` in the sandbox and wait for it to accept
    # requests. Use Start-Process so PowerShell keeps control while the
    # server runs in the background; we kill it in the finally clause.
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = $binary
    # Windows PowerShell 5.1's ProcessStartInfo has no ArgumentList; use the
    # legacy Arguments string. The values are known-safe (no spaces or
    # quotes), so simple space-joining is enough.
    $psi.Arguments = "serve --port $port --quiet --no-store"
    $psi.UseShellExecute = $false
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    # Environment inherits the current process; overwrite the ones we care
    # about so the child never sees the developer's real config paths.
    foreach ($k in $sandboxEnv.Keys) { $psi.EnvironmentVariables[$k] = $sandboxEnv[$k] }

    $server = [System.Diagnostics.Process]::Start($psi)
    try {
        # Wait up to 8 seconds for the loopback port to answer.
        $ready = $false
        for ($i = 0; $i -lt 40; $i++) {
            try {
                $r = Invoke-WebRequest -UseBasicParsing -Uri "http://127.0.0.1:$port/healthz" -TimeoutSec 1
                if ($r.StatusCode -eq 200) { $ready = $true; break }
            } catch { Start-Sleep -Milliseconds 200 }
        }
        if (-not $ready) { throw "aiprobe serve did not respond on port $port" }

        $serveOut = Join-Path $outDir 'serve-sessions.png'
        Write-Host 'Rendering serve-sessions.png...'
        Invoke-Chrome $serveOut "http://127.0.0.1:$port/?lang=en" 1280 900

        $serveRuntimesOut = Join-Path $outDir 'serve-runtimes.png'
        Write-Host 'Rendering serve-runtimes.png...'
        Invoke-Chrome $serveRuntimesOut "http://127.0.0.1:$port/runtimes?lang=en" 1280 900

        $serveOutZh = Join-Path $outDir 'serve-sessions-zh.png'
        Write-Host 'Rendering serve-sessions-zh.png...'
        Invoke-Chrome $serveOutZh "http://127.0.0.1:$port/?lang=zh" 1280 900
    }
    finally {
        if ($server -and -not $server.HasExited) {
            $server.Kill()
            $server.WaitForExit(3000) | Out-Null
        }
        if (Test-Path $sandboxRoot) {
            Remove-Item -Recurse -Force $sandboxRoot -ErrorAction SilentlyContinue
        }
    }

    Write-Host ''
    Write-Host 'Done. Screenshots:'
    Get-ChildItem $outDir -Filter *.png | ForEach-Object {
        '{0,-32} {1,8:N0} bytes' -f $_.Name, $_.Length
    }
}
finally {
    Remove-Item -Recurse -Force $work -ErrorAction SilentlyContinue
}
