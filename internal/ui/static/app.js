// SPDX-License-Identifier: Apache-2.0
// Progressive enhancement only: every page works without JavaScript.
(function () {
  "use strict";

  var filter = document.getElementById("filter");
  function applyFilter() {
    if (!filter) return;
    var q = filter.value.trim().toLowerCase();
    document.querySelectorAll("#live tbody tr[data-text]").forEach(function (tr) {
      tr.hidden = q !== "" && tr.getAttribute("data-text").indexOf(q) === -1;
    });
  }
  if (filter) filter.addEventListener("input", applyFilter);

  var live = document.getElementById("live");
  if (live && live.dataset.src) {
    setInterval(function () {
      if (document.hidden) return;
      fetch(live.dataset.src, { credentials: "same-origin", headers: { Accept: "text/html" } })
        .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
        .then(function (html) { live.innerHTML = html; applyFilter(); })
        .catch(function () { /* keep showing the last good state */ });
    }, 10000);
  }

  document.addEventListener("submit", function (e) {
    var msg = e.target && e.target.getAttribute && e.target.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) e.preventDefault();
  });
})();
