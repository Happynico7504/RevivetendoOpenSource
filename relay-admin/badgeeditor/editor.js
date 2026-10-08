// Badge Arcade badge editor. The badge is a 128x128 artwork (the 3DS also
// derives the 64x64 HOME Menu image and the shadow from it); collision
// outlines are polygons in those 128x128 pixels. All URLs are relative: the
// public site sits behind a path prefix, and the page lives under /my/ so the
// login cookie reaches the API.
(function () {
  "use strict";
  var API = "api/"; // page is /inkay/my/badge-arcade/ on the public site
  var SIZE = 128, ZOOM = 3; // stage = 384px
  var LANGS = [
    [0, "Japanese"], [2, "French"], [3, "German"], [4, "Italian"], [5, "Spanish"],
    [6, "Chinese (Simplified)"], [7, "Korean"], [8, "Dutch"], [9, "Portuguese"],
    [10, "Russian"], [11, "Chinese (Traditional)"]
  ];

  var $ = function (id) { return document.getElementById(id); };
  // T translates through the dashboard's i18n.js (keys in /inkay/i18n/lang/*.js),
  // F fills its {placeholders}; both fall back to the English text.
  var T = function (key, en) { return window.I18N ? I18N.t(key, en) : en; };
  var F = function (key, en, args) { var s = T(key, en); return window.I18N ? I18N.fill(s, args) : s.replace(/\{(\w+)\}/g, function (m, k) { return k in args ? args[k] : m; }); };
  var stage = $("stage"), ctx = stage.getContext("2d");

  var state = {
    img: null,          // HTMLImageElement of the source
    source: null,       // data URL of the (possibly downscaled) source, saved for re-editing
    view: { x: 0, y: 0, zoom: 100, rot: 0 },
    collision: null,    // null = automatic, else manual polygons
    shown: [],          // polygons currently drawn (manual or the last automatic preview)
    current: null,      // {id, status} of the loaded creation
    loggedIn: false,
    dirty: false
  };

  // ---------- helpers ----------
  function api(path, opts) {
    opts = opts || {};
    if (opts.body && typeof opts.body !== "string") {
      opts.body = JSON.stringify(opts.body);
      opts.headers = { "Content-Type": "application/json" };
    }
    opts.credentials = "same-origin";
    return fetch(API + path, opts).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
        return j;
      });
    });
  }
  function status(el, msg, kind) { el.textContent = msg || ""; el.className = "status" + (kind ? " " + kind : ""); }
  function markDirty() { state.dirty = true; }
  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); }

  function loadImage(src) {
    return new Promise(function (resolve, reject) {
      var im = new Image();
      im.onload = function () { resolve(im); };
      im.onerror = function () { reject(new Error(T("be.bad_image", "could not read that image"))); };
      im.src = src;
    });
  }

  // Downscale big uploads (max 1024px) so the saved source stays small.
  function normalizeSource(im) {
    var max = 1024, w = im.naturalWidth, h = im.naturalHeight;
    var c = document.createElement("canvas");
    var s = Math.min(1, max / Math.max(w, h));
    c.width = Math.max(1, Math.round(w * s)); c.height = Math.max(1, Math.round(h * s));
    c.getContext("2d").drawImage(im, 0, 0, c.width, c.height);
    return c.toDataURL("image/png");
  }

  // ---------- drawing ----------
  function baseScale() {
    if (!state.img) return 1;
    var w = state.img.naturalWidth, h = state.img.naturalHeight;
    if (state.view.rot % 2) { var t = w; w = h; h = t; }
    return Math.min(SIZE / w, SIZE / h);
  }
  function drawArt(c, k) {
    c.save();
    c.scale(k, k);
    if (state.img) {
      c.translate(SIZE / 2 + state.view.x, SIZE / 2 + state.view.y);
      c.rotate(state.view.rot * Math.PI / 2);
      var s = baseScale() * state.view.zoom / 100;
      c.scale(s, s);
      c.imageSmoothingQuality = "high";
      c.drawImage(state.img, -state.img.naturalWidth / 2, -state.img.naturalHeight / 2);
    }
    c.restore();
  }
  function render() {
    ctx.clearRect(0, 0, stage.width, stage.height);
    drawArt(ctx, ZOOM);
    if ($("showOutline").checked || $("editOutline").checked) {
      var polys = state.collision || state.shown;
      ctx.lineWidth = 2;
      polys.forEach(function (p) {
        ctx.beginPath();
        p.forEach(function (pt, i) { (i ? ctx.lineTo : ctx.moveTo).call(ctx, pt[0] * ZOOM, pt[1] * ZOOM); });
        ctx.closePath();
        ctx.fillStyle = "rgba(99,102,241,.12)"; ctx.fill();
        ctx.strokeStyle = state.collision ? "#db2777" : "#6366f1"; ctx.stroke();
        if ($("editOutline").checked) {
          p.forEach(function (pt) {
            ctx.beginPath(); ctx.arc(pt[0] * ZOOM, pt[1] * ZOOM, 5, 0, 2 * Math.PI);
            ctx.fillStyle = "#fff"; ctx.fill(); ctx.strokeStyle = "#db2777"; ctx.stroke();
          });
        }
      });
    }
    // badge frame
    ctx.strokeStyle = "rgba(15,23,42,.25)"; ctx.lineWidth = 1;
    ctx.strokeRect(0.5, 0.5, stage.width - 1, stage.height - 1);
  }
  function artDataURL() {
    var c = document.createElement("canvas"); c.width = SIZE; c.height = SIZE;
    drawArt(c.getContext("2d"), 1);
    return c.toDataURL("image/png");
  }

  // ---------- preview ----------
  var previewTimer = null, previewSeq = 0;
  function schedulePreview() {
    clearTimeout(previewTimer);
    previewTimer = setTimeout(preview, 350);
  }
  function preview() {
    if (!state.img) return;
    var seq = ++previewSeq;
    status($("previewStatus"), T("be.rendering", "Rendering…"));
    api("preview", { method: "POST", body: { art: artDataURL(), collision: state.collision } }).then(function (p) {
      if (seq !== previewSeq) return;
      $("p64").src = p.image64; $("p32").src = p.image32;
      $("ptex").src = p.texture; $("pshadow").src = p.shadow;
      state.shown = p.collision || [];
      status($("previewStatus"), state.collision ? T("be.using_outline", "Using your outline.") : F("be.auto_shapes", "Automatic outline: {n} shape(s).", { n: state.shown.length }));
      render();
    }).catch(function (e) { if (seq === previewSeq) status($("previewStatus"), e.message, "err"); });
  }

  // ---------- input: image ----------
  function useFile(file) {
    if (!file) return;
    if (!/^image\/(png|jpeg|gif)$/.test(file.type)) { status($("previewStatus"), T("be.bad_type", "Please use a PNG, JPEG or GIF image."), "err"); return; }
    var reader = new FileReader();
    reader.onload = function () {
      loadImage(reader.result).then(function (im) {
        var src = normalizeSource(im);
        return loadImage(src).then(function (im2) {
          state.img = im2; state.source = src;
          state.view = { x: 0, y: 0, zoom: 100, rot: 0 };
          $("zoom").value = 100;
          $("drophint").classList.add("hidden");
          markDirty(); render(); schedulePreview();
        });
      }).catch(function (e) { status($("previewStatus"), e.message, "err"); });
    };
    reader.readAsDataURL(file);
  }
  $("file").onchange = function () { useFile(this.files[0]); this.value = ""; };
  $("file2").onchange = function () { useFile(this.files[0]); this.value = ""; };
  var dz = $("dropzone");
  dz.addEventListener("dragover", function (e) { e.preventDefault(); dz.classList.add("drag"); });
  dz.addEventListener("dragleave", function () { dz.classList.remove("drag"); });
  dz.addEventListener("drop", function (e) { e.preventDefault(); dz.classList.remove("drag"); useFile(e.dataTransfer.files[0]); });

  $("zoom").oninput = function () { state.view.zoom = +this.value; markDirty(); render(); schedulePreview(); };
  $("fit").onclick = function () { state.view.x = 0; state.view.y = 0; state.view.zoom = 100; $("zoom").value = 100; markDirty(); render(); schedulePreview(); };
  $("rotate").onclick = function () { state.view.rot = (state.view.rot + 1) % 4; markDirty(); render(); schedulePreview(); };
  stage.addEventListener("wheel", function (e) {
    if (!state.img) return;
    e.preventDefault();
    state.view.zoom = Math.max(5, Math.min(400, state.view.zoom * (e.deltaY < 0 ? 1.08 : 1 / 1.08)));
    $("zoom").value = Math.round(state.view.zoom);
    markDirty(); render(); schedulePreview();
  }, { passive: false });

  // ---------- input: dragging (image or outline points) ----------
  var drag = null;
  function stagePoint(e) {
    var r = stage.getBoundingClientRect();
    return [(e.clientX - r.left) * SIZE / r.width, (e.clientY - r.top) * SIZE / r.height];
  }
  stage.addEventListener("pointerdown", function (e) {
    var pt = stagePoint(e);
    if ($("editOutline").checked && state.collision) {
      var best = null, bestD = 4; // 128-px units (~12 screen px)
      state.collision.forEach(function (poly, pi) {
        poly.forEach(function (v, vi) {
          var d = Math.hypot(v[0] - pt[0], v[1] - pt[1]);
          if (d < bestD) { bestD = d; best = [pi, vi]; }
        });
      });
      if (best) { drag = { kind: "point", pi: best[0], vi: best[1] }; stage.setPointerCapture(e.pointerId); return; }
    }
    if (state.img && !$("editOutline").checked) {
      drag = { kind: "pan", start: pt, x: state.view.x, y: state.view.y };
      stage.setPointerCapture(e.pointerId);
    }
  });
  stage.addEventListener("pointermove", function (e) {
    if (!drag) return;
    var pt = stagePoint(e);
    if (drag.kind === "pan") {
      state.view.x = drag.x + pt[0] - drag.start[0];
      state.view.y = drag.y + pt[1] - drag.start[1];
    } else {
      state.collision[drag.pi][drag.vi] = [Math.max(0, Math.min(SIZE, pt[0])), Math.max(0, Math.min(SIZE, pt[1]))];
    }
    markDirty(); render();
  });
  stage.addEventListener("pointerup", function () { if (drag) { drag = null; schedulePreview(); } });

  // ---------- outline controls ----------
  $("showOutline").onchange = render;
  $("editOutline").onchange = function () {
    stage.classList.toggle("editing", this.checked);
    if (this.checked && !state.collision) {
      state.collision = JSON.parse(JSON.stringify(state.shown)); // start from the automatic outline
      markDirty();
    }
    render();
  };
  $("autoOutline").onclick = function () {
    state.collision = null; $("editOutline").checked = false; stage.classList.remove("editing");
    markDirty(); render(); schedulePreview();
  };

  // ---------- names ----------
  var langBox = $("langs");
  LANGS.forEach(function (l) {
    var id = "name" + l[0];
    var wrap = document.createElement("div");
    wrap.innerHTML = '<label for="' + id + '" data-i18n="be.lang' + l[0] + '">' + esc(l[1]) + '</label><textarea id="' + id + '" rows="2" maxlength="120" placeholder="uses English" data-i18n-placeholder="be.uses_english"></textarea>';
    langBox.appendChild(wrap);
  });
  document.querySelectorAll(".names textarea").forEach(function (t) {
    t.addEventListener("input", markDirty);
    t.addEventListener("keydown", function (e) { // at most 2 lines
      if (e.key === "Enter" && t.value.split("\n").length >= 2) e.preventDefault();
    });
  });
  function names() {
    var n = []; for (var i = 0; i < 16; i++) n.push("");
    n[1] = $("nameEn").value;
    LANGS.forEach(function (l) { n[l[0]] = $("name" + l[0]).value; });
    return n;
  }
  function setNames(n) {
    var en = (n[1] || "").replace(/\r\n/g, "\n");
    $("nameEn").value = en;
    LANGS.forEach(function (l) {
      var v = (n[l[0]] || "").replace(/\r\n/g, "\n");
      $("name" + l[0]).value = v === en ? "" : v;
    });
  }

  // ---------- account & creations ----------
  function refreshAccount() {
    return api("me").then(function (me) {
      state.loggedIn = me.loggedIn;
      var a = $("account");
      if (me.loggedIn) {
        a.innerHTML = esc(T("be.logged_in_as", "Logged in as")) + " <strong>" + esc(me.pnid || "you") + "</strong> · <a href=\"../\">" + esc(T("be.my_page", "My page")) + "</a>" +
          (me.banned ? '<br><span class="note">' + esc(T("be.banned", "This account can't save creations.")) + "</span>" : "");
      } else {
        a.innerHTML = T("be.login_prompt", '<a href="../">Log in</a> to save your creations<br><small>(then come back to this page)</small>');
      }
      $("save").disabled = $("saveCopy").disabled = !me.loggedIn;
      updateSubmit();
      return refreshList();
    });
  }
  function refreshList() {
    var box = $("creations");
    if (!state.loggedIn) { box.innerHTML = '<p class="hint">' + T("be.login_to_see", "Log in to see your saved creations.") + "</p>"; return Promise.resolve(); }
    return api("creations").then(function (list) {
      if (!list.length) { box.innerHTML = '<p class="hint">' + T("be.nothing", "Nothing saved yet.") + "</p>"; return; }
      box.innerHTML = '<div class="list">' + list.map(function (c) {
        var cur = state.current && state.current.id === c.id ? " current" : "";
        return '<div class="item' + cur + '"><img src="' + API + "creations/" + c.id + "/art.png?v=" + encodeURIComponent(c.updatedAt) + '" alt="">' +
          '<div class="t" title="' + esc(c.title) + '">' + esc(c.title) + "</div>" +
          '<span class="pill ' + c.status + '">' + esc(T("be.st_" + c.status, c.status)) + "</span>" +
          (c.reviewNote ? '<div class="note">' + esc(c.reviewNote) + "</div>" : "") +
          '<div class="actions"><button type="button" data-load="' + c.id + '">' + (c.status === "draft" || c.status === "rejected" ? T("be.edit", "Edit") : T("be.view", "View")) + "</button>" +
          (c.status === "submitted" ? '<button type="button" data-withdraw="' + c.id + '">' + T("be.withdraw", "Withdraw") + "</button>" : "") +
          (c.status !== "approved" ? '<button type="button" data-del="' + c.id + '">' + T("be.delete", "Delete") + "</button>" : "") + "</div></div>";
      }).join("") + "</div>";
    }).catch(function (e) { box.innerHTML = '<p class="status err">' + esc(e.message) + "</p>"; });
  }
  $("creations").addEventListener("click", function (e) {
    var id = e.target.getAttribute("data-load"), del = e.target.getAttribute("data-del"), wd = e.target.getAttribute("data-withdraw");
    if (wd) {
      api("creations/" + wd + "/withdraw", { method: "POST", body: {} }).then(function () {
        if (state.current && state.current.id === +wd) state.current.status = "draft";
        refreshList(); updateSubmit();
      }).catch(function (err) { alert(err.message); });
      return;
    }
    if (id) {
      if (state.dirty && !confirm(T("be.discard", "Discard your unsaved changes?"))) return;
      loadCreation(+id);
    } else if (del && confirm(T("be.confirm_delete", "Delete this creation? This can't be undone."))) {
      api("creations/" + del, { method: "DELETE" }).then(function () {
        if (state.current && state.current.id === +del) state.current = null;
        refreshList();
      }).catch(function (err) { alert(err.message); });
    }
  });

  function loadCreation(id) {
    status($("saveStatus"), T("be.loading", "Loading…"));
    api("creations/" + id).then(function (c) {
      var spec = c.spec || {};
      var view = spec.view || null;
      var src = c.source || c.art;
      return loadImage(src).then(function (im) {
        state.img = im; state.source = c.source || null;
        state.view = view && c.source ? view : { x: 0, y: 0, zoom: 100, rot: 0 };
        $("zoom").value = Math.round(state.view.zoom);
        state.collision = spec.collision && spec.collision.length ? spec.collision : null;
        $("editOutline").checked = false; stage.classList.remove("editing");
        setNames(spec.names || []);
        $("title").value = c.title || "";
        state.current = { id: c.id, status: c.status };
        state.dirty = false;
        updateSubmit();
        $("drophint").classList.add("hidden");
        render(); preview(); refreshList();
        status($("saveStatus"), c.status === "submitted" || c.status === "approved"
          ? F("be.locked", "This creation is {status}; “Save as copy” to change it.", { status: T("be.st_" + c.status, c.status) }) : T("be.loaded", "Loaded."));
      });
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); });
  }

  function save(asCopy) {
    if (!state.img) { status($("saveStatus"), T("be.need_image", "Add an image first."), "err"); return Promise.reject(new Error("no image")); }
    if (!$("nameEn").value.trim()) { status($("saveStatus"), T("be.need_name", "Give the badge an English name."), "err"); $("nameEn").focus(); return Promise.reject(new Error("no name")); }
    var body = {
      kind: "badge", title: $("title").value, art: artDataURL(), source: state.source || "",
      spec: { names: names(), collision: state.collision, view: state.view }
    };
    var editable = state.current && (state.current.status === "draft" || state.current.status === "rejected");
    var req = !asCopy && editable
      ? api("creations/" + state.current.id, { method: "PUT", body: body })
      : api("creations", { method: "POST", body: body });
    status($("saveStatus"), T("be.saving", "Saving…"));
    return req.then(function (r) {
      state.current = { id: r.id, status: "draft" };
      state.dirty = false;
      status($("saveStatus"), T("be.saved", "Saved."), "ok");
      refreshList(); updateSubmit();
      return r;
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); throw e; });
  }

  // ---------- submitting for review ----------
  function updateSubmit() {
    var st = state.current && state.current.status;
    var b = $("submit");
    b.disabled = !state.loggedIn || st === "submitted" || st === "approved";
    b.textContent = st === "submitted" ? T("be.waiting", "Waiting for review") : st === "approved" ? T("be.st_approved", "Approved") : T("be.submit_review", "Submit for review…");
  }
  $("submit").onclick = function () {
    if (!state.img) { status($("saveStatus"), T("be.need_image", "Add an image first."), "err"); return; }
    $("rules").hidden = false;
  };
  $("submitCancel").onclick = function () { $("rules").hidden = true; };
  $("submitConfirm").onclick = function () {
    $("rules").hidden = true;
    var st = state.current && state.current.status;
    var saved = state.current && !state.dirty && (st === "draft" || st === "rejected")
      ? Promise.resolve(state.current) : save(false);
    saved.then(function (c) {
      return api("creations/" + c.id + "/submit", { method: "POST", body: {} });
    }).then(function () {
      state.current.status = "submitted";
      status($("saveStatus"), T("be.submitted", "Submitted — you'll see the review result in “My creations”."), "ok");
      refreshList(); updateSubmit();
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); });
  };
  $("save").onclick = function () { save(false).catch(function () {}); };
  $("saveCopy").onclick = function () { save(true).catch(function () {}); };
  $("new").onclick = function () {
    if (state.dirty && !confirm(T("be.discard", "Discard your unsaved changes?"))) return;
    state.img = null; state.source = null; state.collision = null; state.shown = []; state.current = null; state.dirty = false;
    state.view = { x: 0, y: 0, zoom: 100, rot: 0 }; $("zoom").value = 100;
    setNames([]); $("title").value = "";
    ["p64", "p32", "ptex", "pshadow"].forEach(function (i) { $(i).removeAttribute("src"); });
    $("drophint").classList.remove("hidden");
    status($("saveStatus"), ""); status($("previewStatus"), "");
    render(); refreshList(); updateSubmit();
  };

  window.addEventListener("beforeunload", function (e) { if (state.dirty) { e.preventDefault(); e.returnValue = ""; } });

  function start() {
    render();
    refreshAccount().catch(function () { $("account").textContent = ""; });
  }
  if (window.I18N) I18N.ready(start); else start();
})();
