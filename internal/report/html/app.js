/* AIProbe single-file report behaviour.
   Vanilla JS, inlined into the document. No framework, no network access.
   Everything degrades gracefully: with JS disabled all panels stay readable
   because the print stylesheet reveals them, and tables are already sorted by
   the most useful column on the server side. */

(function () {
  "use strict";

  function ready(fn) {
    if (document.readyState !== "loading") { fn(); return; }
    document.addEventListener("DOMContentLoaded", fn);
  }

  /* ---- tabs ---- */

  function initTabs() {
    var tabs = Array.prototype.slice.call(document.querySelectorAll(".tab"));
    var panels = Array.prototype.slice.call(document.querySelectorAll(".panel"));
    if (!tabs.length) { return; }

    function select(id, pushHash) {
      tabs.forEach(function (t) {
        var on = t.getAttribute("data-target") === id;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
      });
      panels.forEach(function (p) {
        p.classList.toggle("active", p.id === id);
      });
      if (pushHash && window.history && window.history.replaceState) {
        window.history.replaceState(null, "", "#" + id);
      }
    }

    tabs.forEach(function (t, i) {
      t.addEventListener("click", function () {
        select(t.getAttribute("data-target"), true);
      });
      t.addEventListener("keydown", function (e) {
        var next = -1;
        if (e.key === "ArrowRight") { next = (i + 1) % tabs.length; }
        if (e.key === "ArrowLeft") { next = (i - 1 + tabs.length) % tabs.length; }
        if (next < 0) { return; }
        e.preventDefault();
        tabs[next].focus();
        select(tabs[next].getAttribute("data-target"), true);
      });
    });

    var initial = (window.location.hash || "").replace("#", "");
    var known = panels.some(function (p) { return p.id === initial; });
    select(known ? initial : tabs[0].getAttribute("data-target"), false);
  }

  /* ---- sortable tables ---- */

  function cellValue(row, index) {
    var cell = row.children[index];
    if (!cell) { return ""; }
    var raw = cell.getAttribute("data-sort");
    if (raw === null) { raw = cell.textContent; }
    return raw.trim();
  }

  function comparator(index, numeric, dir) {
    return function (a, b) {
      var av = cellValue(a, index);
      var bv = cellValue(b, index);
      var res;
      if (numeric) {
        var an = parseFloat(av.replace(/[^0-9.\-]/g, ""));
        var bn = parseFloat(bv.replace(/[^0-9.\-]/g, ""));
        if (isNaN(an)) { an = -Infinity; }
        if (isNaN(bn)) { bn = -Infinity; }
        res = an - bn;
      } else {
        res = av.localeCompare(bv);
      }
      return dir === "descending" ? -res : res;
    };
  }

  function initSort() {
    Array.prototype.forEach.call(document.querySelectorAll("table.data"), function (table) {
      var headers = Array.prototype.slice.call(table.querySelectorAll("th.sortable"));
      headers.forEach(function (th, index) {
        var colIndex = Array.prototype.indexOf.call(th.parentNode.children, th);
        th.setAttribute("tabindex", "0");
        th.setAttribute("role", "columnheader");

        function apply() {
          var current = th.getAttribute("aria-sort");
          var dir = current === "descending" ? "ascending" : "descending";
          headers.forEach(function (h) { h.removeAttribute("aria-sort"); });
          th.setAttribute("aria-sort", dir);
          var body = table.tBodies[0];
          if (!body) { return; }
          var rows = Array.prototype.slice.call(body.rows);
          var numeric = th.getAttribute("data-numeric") === "true";
          rows.sort(comparator(colIndex, numeric, dir));
          rows.forEach(function (r) { body.appendChild(r); });
        }

        th.addEventListener("click", apply);
        th.addEventListener("keydown", function (e) {
          if (e.key === "Enter" || e.key === " ") { e.preventDefault(); apply(); }
        });
      });
    });
  }

  /* ---- copy JSON ---- */

  function initCopy() {
    var btn = document.getElementById("copy-json");
    var src = document.getElementById("report-json");
    if (!btn || !src) { return; }
    btn.addEventListener("click", function () {
      var text = src.textContent;
      var done = function () {
        var old = btn.textContent;
        btn.textContent = "copied";
        setTimeout(function () { btn.textContent = old; }, 1400);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(done, function () { fallback(text, done); });
        return;
      }
      fallback(text, done);
    });

    function fallback(text, done) {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.setAttribute("readonly", "");
      ta.style.position = "absolute";
      ta.style.left = "-9999px";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); done(); } catch (err) { /* clipboard unavailable */ }
      document.body.removeChild(ta);
    }
  }

  ready(function () {
    initTabs();
    initSort();
    initCopy();
  });
})();
