/* AIProbe live report behaviour.
   Inlined only into report pages the local server renders, never into a file.
   It talks to that server alone, by relative address, and writes to the page
   through textContent only: a preview is prompt text, and the page that shows
   it is served from this machine. */

(function () {
  "use strict";

  if (!window.fetch) { return; }

  function ready(fn) {
    if (document.readyState !== "loading") { fn(); return; }
    document.addEventListener("DOMContentLoaded", fn);
  }

  /* ---- previews ----
     The first paint carries fallback labels; the previews arrive afterwards.
     They are applied only when the response describes the bytes the page was
     rendered from: the same fingerprint and the same number of questions. A
     transcript that grew after first paint can renumber its questions without
     changing how many there are, so the count alone is not enough. Any mismatch,
     and any failure, leaves the fallback labels in place. */

  function loadPreviews() {
    var panel = document.getElementById("panel-questions");
    if (!panel) { return; }
    var href = panel.getAttribute("data-previews");
    if (!href) { return; }
    var fp = panel.getAttribute("data-fp") || "";
    var count = parseInt(panel.getAttribute("data-count") || "", 10);

    fetch(href)
      .then(function (r) { return r.ok ? r.json() : null; })
      .then(function (data) {
        if (!data || data.fingerprint !== fp || data.count !== count || !data.previews) { return; }
        var cells = panel.querySelectorAll(".q-preview[data-q]");
        for (var i = 0; i < cells.length; i++) {
          var n = cells[i].getAttribute("data-q");
          if (!Object.prototype.hasOwnProperty.call(data.previews, n)) { continue; }
          var text = data.previews[n];
          cells[i].textContent = text ? text : (cells[i].getAttribute("data-empty") || "");
        }
      })
      .catch(function () { /* keep the fallback labels */ });
  }

  /* ---- re-analysis ----
     Re-runs an analysis that predates the questions section. The request sets no
     mode on purpose: a same-origin POST in the default mode carries this page's
     real Origin, which the server checks, whereas mode "same-origin" would send
     Origin: null under this page's no-referrer policy and be refused. */

  function initReanalyse() {
    var btn = document.querySelector("button[data-reanalyse]");
    if (!btn) { return; }
    var status = document.getElementById("q-reanalyse-status");

    function say(text) {
      if (status) { status.textContent = text; }
    }

    function fail(msg) {
      btn.disabled = false;
      var head = btn.getAttribute("data-failed") || "";
      say(msg ? head + " " + msg : head);
    }

    btn.addEventListener("click", function () {
      var href = btn.getAttribute("data-reanalyse");
      if (!href) { return; }
      btn.disabled = true;
      say(btn.getAttribute("data-working") || "");
      fetch(href, { method: "POST" })
        .then(function (r) {
          return r.json().then(
            function (body) { return body || {}; },
            function () { return { error: "HTTP " + r.status }; });
        })
        .then(function (body) {
          if (body.ok === true) {
            // Changing only the hash would not reload, so reload explicitly: the
            // new GET is served the analysis just written to the store.
            if (window.history && window.history.replaceState) {
              window.history.replaceState(null, "", "#panel-questions");
            }
            window.location.reload();
            return;
          }
          fail(body.error || "");
        })
        .catch(function (e) { fail(e && e.message ? e.message : String(e)); });
    });
  }

  ready(function () {
    loadPreviews();
    initReanalyse();
  });
})();
