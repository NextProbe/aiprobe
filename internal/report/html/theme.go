package html

import "html/template"

// Light and dark themes
//
// The palette lives in CSS custom properties, so switching themes is a matter of
// setting one attribute on the document element. Three things made this more than
// a one-liner.
//
// The default has to be the reader's system preference, not ours. A report opened
// on a machine set to light mode should be light, without anyone having to ask.
//
// An explicit choice has to win over the system, and survive a reload. That means
// storage, and storage means the choice has to be applied before the first paint
// or the reader sees a flash of the wrong theme. Hence an inline script in <head>
// rather than logic in app.js, which runs at the end of the document.
//
// The same control has to work on four page types: the single-session report, the
// cross-session aggregate, and the server's session list and history pages. The
// last two do not load app.js at all, so the toggle logic lives here, in the one
// snippet all four include.

// themeStorageKey is where an explicit choice is remembered. Namespaced because a
// report may be opened from the same origin as other local files.
const themeStorageKey = "aiprobe-theme"

// themeJS applies a stored theme immediately and wires the toggle once the button
// exists.
//
// Written for the document head, so it must not assume the DOM is ready. Every
// storage access is guarded: localStorage throws rather than returning null in a
// few sandboxed contexts, and a theme preference is not worth breaking a page for.
const themeJS = `(function () {
  "use strict";
  var KEY = "` + themeStorageKey + `";
  var root = document.documentElement;

  function stored() {
    try { return localStorage.getItem(KEY); } catch (e) { return null; }
  }
  function remember(v) {
    try { localStorage.setItem(KEY, v); } catch (e) { /* private mode: honour it for this page only */ }
  }
  function systemPrefersLight() {
    return !!(window.matchMedia && window.matchMedia("(prefers-color-scheme: light)").matches);
  }
  // The effective theme: an explicit choice, else what the system asks for.
  function current() {
    var s = stored();
    if (s === "light" || s === "dark") { return s; }
    return systemPrefersLight() ? "light" : "dark";
  }

  // Applied before first paint, which is why this script is inline in the head.
  var initial = stored();
  if (initial === "light" || initial === "dark") {
    root.setAttribute("data-theme", initial);
  }

  function paintButton(btn) {
    var now = current();
    var icon = btn.querySelector(".icon");
    var label = btn.querySelector(".label");
    // The button offers the other theme rather than naming the current one: a
    // control should say what it does, not what already happened.
    var next = now === "light" ? "dark" : "light";
    if (icon) { icon.textContent = next === "light" ? "\u2600" : "\u263D"; }
    if (label) { label.textContent = btn.getAttribute("data-label-" + next) || next; }
    btn.setAttribute("aria-pressed", now === "light" ? "true" : "false");
  }

  function init() {
    var btn = document.getElementById("theme-toggle");
    if (!btn) { return; }
    paintButton(btn);
    btn.addEventListener("click", function () {
      var next = current() === "light" ? "dark" : "light";
      root.setAttribute("data-theme", next);
      remember(next);
      paintButton(btn);
    });
    // Follow the system while no explicit choice has been made.
    if (window.matchMedia) {
      var mq = window.matchMedia("(prefers-color-scheme: light)");
      var onChange = function () { if (!stored()) { paintButton(btn); } };
      if (mq.addEventListener) { mq.addEventListener("change", onChange); }
      else if (mq.addListener) { mq.addListener(onChange); }
    }
  }

  if (document.readyState !== "loading") { init(); }
  else { document.addEventListener("DOMContentLoaded", init); }
})();`

// ThemeJS returns the theme script for a document head.
func ThemeJS() template.JS { return template.JS(themeJS) }

// Stylesheet returns the report stylesheet.
//
// Exported so the server's own pages, which carry a smaller stylesheet of their
// own, can be checked against this one for palette drift.
func Stylesheet() string {
	b, err := assets.ReadFile("style.css")
	if err != nil {
		return ""
	}
	return string(b)
}

// ThemeToggle returns the toggle button.
//
// The labels are passed in already localized. They travel as data attributes so
// the script itself carries no English: it swaps between whichever two words the
// caller supplied.
func ThemeToggle(ariaLabel, light, dark string) template.HTML {
	return template.HTML(`<button class="theme-toggle" id="theme-toggle" type="button"` +
		` aria-label="` + template.HTMLEscapeString(ariaLabel) + `"` +
		` data-label-light="` + template.HTMLEscapeString(light) + `"` +
		` data-label-dark="` + template.HTMLEscapeString(dark) + `"` +
		`><span class="icon" aria-hidden="true"></span><span class="label"></span></button>`)
}
