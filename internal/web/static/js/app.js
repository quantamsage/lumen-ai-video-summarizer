// Progressive enhancements. Every page works without JavaScript; this file
// adds upload progress, live processing status, timestamp seeking, quiz
// paging, clipboard copy and the theme toggle.
(function () {
  "use strict";

  var $ = function (sel, root) { return (root || document).querySelector(sel); };
  var $$ = function (sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); };
  var csrf = (function () { var m = $('meta[name="csrf-token"]'); return m ? m.content : ""; })();

  // ---- Navigation and theme -------------------------------------------------
  var navToggle = $("[data-nav-toggle]");
  if (navToggle) {
    navToggle.addEventListener("click", function () {
      var open = document.body.classList.toggle("nav-open");
      navToggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
    var scrim = $(".scrim");
    if (scrim) scrim.addEventListener("click", function () { document.body.classList.remove("nav-open"); });
  }

  $$("[data-theme-toggle]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var root = document.documentElement;
      var current = root.getAttribute("data-theme") ||
        (window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
      var next = current === "dark" ? "light" : "dark";
      root.setAttribute("data-theme", next);
      try { localStorage.setItem("theme", next); } catch (e) { /* ignore */ }
    });
  });

  // ---- Confirmations ----------------------------------------------------------
  $$("form[data-confirm]").forEach(function (form) {
    form.addEventListener("submit", function (e) {
      if (!window.confirm(form.getAttribute("data-confirm"))) e.preventDefault();
    });
  });

  // Disable submit buttons after submission to prevent duplicates.
  $$("form[data-once]").forEach(function (form) {
    form.addEventListener("submit", function () {
      $$("button[type=submit]", form).forEach(function (b) {
        setTimeout(function () { b.disabled = true; }, 0);
      });
    });
  });

  // ---- Clipboard ----------------------------------------------------------------
  $$("[data-copy-target]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var target = document.getElementById(btn.getAttribute("data-copy-target"));
      if (!target) return;
      var text = target.value !== undefined ? target.value : target.innerText;
      var label = btn.textContent;
      var done = function (ok) {
        btn.textContent = ok ? "Copied!" : "Copy failed";
        setTimeout(function () { btn.textContent = label; }, 1800);
      };
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
      } else {
        done(false);
      }
    });
  });

  // Copy text fetched from a URL (used for long transcripts so they are not
  // embedded in the page).
  $$("[data-copy-url]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var label = btn.textContent;
      var done = function (ok) {
        btn.textContent = ok ? "Copied!" : "Copy failed";
        setTimeout(function () { btn.textContent = label; }, 1800);
      };
      if (!navigator.clipboard || !navigator.clipboard.writeText) { done(false); return; }
      fetch(btn.getAttribute("data-copy-url"), { credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.text(); })
        .then(function (t) { return navigator.clipboard.writeText(t); })
        .then(function () { done(true); }, function () { done(false); });
    });
  });

  // ---- Tabs (add video page) ------------------------------------------------------
  $$("[data-tabs]").forEach(function (tabs) {
    var buttons = $$("[data-tab]", tabs);
    buttons.forEach(function (b) {
      b.addEventListener("click", function (e) {
        e.preventDefault();
        buttons.forEach(function (o) {
          var sel = o === b;
          o.setAttribute("aria-selected", sel ? "true" : "false");
          var panel = document.getElementById(o.getAttribute("data-tab"));
          if (panel) panel.classList.toggle("hidden", !sel);
        });
        if (history.replaceState) history.replaceState(null, "", b.getAttribute("href"));
      });
    });
  });

  // ---- File pickers / dropzones -----------------------------------------------------
  $$("[data-dropzone]").forEach(function (zone) {
    var input = $("input[type=file]", zone);
    var label = $("[data-file-label]", zone);
    var show = function () {
      if (label) label.textContent = input.files.length ? input.files[0].name + " (" + formatBytes(input.files[0].size) + ")" : "";
    };
    input.addEventListener("change", show);
    ["dragenter", "dragover"].forEach(function (ev) {
      zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.add("dragover"); });
    });
    ["dragleave", "drop"].forEach(function (ev) {
      zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.remove("dragover"); });
    });
    zone.addEventListener("drop", function (e) {
      if (e.dataTransfer && e.dataTransfer.files.length) {
        input.files = e.dataTransfer.files;
        show();
      }
    });
  });

  function formatBytes(n) {
    if (n < 1024) return n + " B";
    var units = ["KB", "MB", "GB"], i = -1;
    do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
    return n.toFixed(1) + " " + units[i];
  }

  // ---- Upload with progress -------------------------------------------------------
  $$("form[data-upload]").forEach(function (form) {
    var panel = $("[data-upload-progress]", form);
    var bar = panel && $("progress", panel);
    var pct = panel && $("[data-upload-percent]", panel);
    var status = panel && $("[data-upload-status]", panel);
    var errorBox = $("[data-upload-error]", form);
    var submit = $("button[type=submit]", form);
    var maxBytes = parseInt(form.getAttribute("data-max-bytes") || "0", 10);

    form.addEventListener("submit", function (e) {
      if (!window.FormData || !window.XMLHttpRequest) return;
      var fileInput = $("input[name=video]", form);
      if (fileInput && fileInput.files.length && maxBytes && fileInput.files[0].size > maxBytes) {
        e.preventDefault();
        showError("This file is larger than the " + Math.round(maxBytes / 1048576) + " MB limit.");
        return;
      }
      e.preventDefault();
      hideError();
      var xhr = new XMLHttpRequest();
      xhr.open("POST", form.action);
      xhr.setRequestHeader("Accept", "application/json");
      xhr.setRequestHeader("X-CSRF-Token", csrf);
      if (panel) panel.classList.remove("hidden");
      submit.disabled = true;
      xhr.upload.addEventListener("progress", function (ev) {
        if (!ev.lengthComputable || !bar) return;
        var p = Math.round((ev.loaded / ev.total) * 100);
        bar.value = p;
        pct.textContent = p + "%";
        status.textContent = p < 100 ? "Uploading " + formatBytes(ev.loaded) + " of " + formatBytes(ev.total) : "Finishing up…";
      });
      xhr.addEventListener("load", function () {
        var data = {};
        try { data = JSON.parse(xhr.responseText); } catch (err) { /* non-JSON */ }
        if (xhr.status >= 200 && xhr.status < 300 && data.redirect) {
          window.location.href = data.redirect;
          return;
        }
        if (xhr.status === 413) data.error = data.error || "The file is too large.";
        if (xhr.status === 429) data.error = data.error || "Too many uploads. Please wait and try again.";
        fail(data.error || "The upload failed (HTTP " + xhr.status + "). Please try again.");
      });
      xhr.addEventListener("error", function () { fail("The connection was lost during upload. Check your network and try again."); });
      xhr.addEventListener("abort", function () { fail("The upload was cancelled."); });
      xhr.send(new FormData(form));

      var cancel = $("[data-upload-cancel]", form);
      if (cancel) {
        cancel.onclick = function () { xhr.abort(); };
      }
    });

    function fail(msg) {
      if (panel) panel.classList.add("hidden");
      submit.disabled = false;
      showError(msg);
    }
    function showError(msg) {
      if (!errorBox) return;
      errorBox.textContent = msg;
      errorBox.classList.remove("hidden");
    }
    function hideError() { if (errorBox) errorBox.classList.add("hidden"); }
  });

  // ---- Live processing status -----------------------------------------------------
  $$("[data-poll-url]").forEach(function (el) {
    var url = el.getAttribute("data-poll-url");
    var initial = el.getAttribute("data-initial") || "";
    var bar = $("progress", el);
    var stage = $("[data-stage]", el);
    var percent = $("[data-percent]", el);
    var delay = 2500;
    var tick = function () {
      fetch(url, { headers: { Accept: "application/json" }, credentials: "same-origin" })
        .then(function (r) { if (!r.ok) throw new Error(r.status); return r.json(); })
        .then(function (d) {
          var key = d.status + "/" + d.summary_status + "/" + d.quiz_status;
          if (key !== initial) { window.location.reload(); return; }
          var p = d.quiz_progress !== undefined && d.status === "ready" ? d.quiz_progress : d.progress;
          var s = d.quiz_stage !== undefined && d.status === "ready" ? d.quiz_stage : d.stage;
          if (bar && typeof p === "number") bar.value = p;
          if (percent && typeof p === "number") percent.textContent = p + "%";
          if (stage && s) stage.textContent = s;
          delay = 2500;
          setTimeout(tick, delay);
        })
        .catch(function () { delay = Math.min(delay * 2, 30000); setTimeout(tick, delay); });
    };
    setTimeout(tick, delay);
  });

  // ---- Timestamp seeking ------------------------------------------------------------
  var video = $("#player video");
  var embed = $("#player iframe[data-embed-base]");
  document.addEventListener("click", function (e) {
    var link = e.target.closest && e.target.closest("a[data-seek]");
    if (!link) return;
    var t = parseFloat(link.getAttribute("data-seek"));
    if (isNaN(t)) return;
    if (video) {
      e.preventDefault();
      video.currentTime = t;
      var p = video.play();
      if (p && p.catch) p.catch(function () { /* autoplay blocked; user can press play */ });
      $("#player").scrollIntoView({ behavior: "smooth", block: "center" });
    } else if (embed) {
      e.preventDefault();
      var base = embed.getAttribute("data-embed-base");
      var kind = embed.getAttribute("data-embed-kind");
      var secs = Math.floor(t);
      embed.src = kind === "vimeo" ? base + "?autoplay=1#t=" + secs + "s" : base + "?start=" + secs + "&autoplay=1";
      $("#player").scrollIntoView({ behavior: "smooth", block: "center" });
    }
  });

  // Highlight the transcript segment being played.
  if (video) {
    var segs = $$(".seg[data-start]").map(function (el) {
      return { el: el, start: parseFloat(el.getAttribute("data-start")), end: parseFloat(el.getAttribute("data-end")) };
    });
    var active = null;
    video.addEventListener("timeupdate", function () {
      var t = video.currentTime, found = null;
      for (var i = 0; i < segs.length; i++) {
        if (t >= segs[i].start && t < segs[i].end) { found = segs[i].el; break; }
      }
      if (found !== active) {
        if (active) active.classList.remove("active");
        if (found) found.classList.add("active");
        active = found;
      }
    });
  }

  // ---- Score ring ------------------------------------------------------------------
  $$(".score-ring[data-score]").forEach(function (el) {
    el.style.setProperty("--p", el.getAttribute("data-score"));
  });

  // ---- Quiz: one question at a time -------------------------------------------------
  var quiz = $("form[data-quiz]");
  if (quiz) {
    var questions = $$(".question", quiz);
    var prev = $("[data-quiz-prev]", quiz);
    var next = $("[data-quiz-next]", quiz);
    var submitBtn = $("[data-quiz-submit]", quiz);
    var counter = $("[data-quiz-counter]", quiz);
    var progress = $("[data-quiz-progress]", quiz);
    var dotsBox = $("[data-quiz-dots]", quiz);
    var idx = 0;
    quiz.classList.add("quiz-paged");

    var dots = questions.map(function (q, i) {
      var d = document.createElement("button");
      d.type = "button";
      d.className = "dot";
      d.textContent = String(i + 1);
      d.setAttribute("aria-label", "Go to question " + (i + 1));
      d.addEventListener("click", function () { show(i); });
      dotsBox.appendChild(d);
      return d;
    });

    function answered(q) {
      if ($$("input:checked", q).length) return true;
      var t = $("textarea, input[type=text]", q);
      return !!(t && t.value.trim());
    }
    function refresh() {
      var count = 0;
      questions.forEach(function (q, i) {
        var a = answered(q);
        if (a) count++;
        dots[i].classList.toggle("answered", a);
        dots[i].classList.toggle("current", i === idx);
      });
      progress.value = Math.round((count / questions.length) * 100);
      counter.textContent = "Question " + (idx + 1) + " of " + questions.length + " · " + count + " answered";
      prev.disabled = idx === 0;
      next.classList.toggle("hidden", idx === questions.length - 1);
      submitBtn.classList.toggle("hidden", idx !== questions.length - 1);
    }
    function show(i) {
      idx = Math.max(0, Math.min(questions.length - 1, i));
      questions.forEach(function (q, j) { q.classList.toggle("current", j === idx); });
      refresh();
      var focusable = $("input, textarea", questions[idx]);
      if (focusable) focusable.focus({ preventScroll: true });
    }
    prev.addEventListener("click", function () { show(idx - 1); });
    next.addEventListener("click", function () { show(idx + 1); });
    quiz.addEventListener("input", refresh);
    quiz.addEventListener("change", refresh);
    quiz.addEventListener("submit", function (e) {
      var missing = questions.filter(function (q) { return !answered(q); }).length;
      if (missing && !window.confirm(missing + " question(s) are unanswered. Submit anyway?")) {
        e.preventDefault();
        return;
      }
      submitBtn.disabled = true;
    });
    document.addEventListener("keydown", function (e) {
      if (e.target.tagName === "TEXTAREA" || e.target.tagName === "INPUT" && e.target.type === "text") return;
      if (e.key === "ArrowRight") show(idx + 1);
      if (e.key === "ArrowLeft") show(idx - 1);
    });
    show(0);
  }
})();
