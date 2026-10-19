// Badge Arcade stage editor. A stage is the background of a crane machine: a
// 400x240 image for the 3DS top screen plus the colour of the narrow rail the
// game draws from the same texture (see badgearcade/crb.go). The image always
// covers the screen; zoom and drag choose the crop. Shares the creations API
// and the "My creations" list with the badge editor (index.html).
(function () {
  "use strict";
  var API = "api/";
  var W = 400, H = 240, ZOOM = 2; // canvas = 800x480
  var DEFAULT_TINT = "#9ca3af";

  var PAGES = { badge: "./", stage: "stage.html", crane: "layout.html" };
  var $ = function (id) { return document.getElementById(id); };
  var T = function (key, en) { return window.I18N ? I18N.t(key, en) : en; };
  var F = function (key, en, args) { var s = T(key, en); return window.I18N ? I18N.fill(s, args) : s.replace(/\{(\w+)\}/g, function (m, k) { return k in args ? args[k] : m; }); };
  var stage = $("stage"), ctx = stage.getContext("2d");

  var state = {
    img: null,
    source: null,                 // data URL of the (downscaled) upload, saved for re-editing
    view: { x: 0, y: 0, zoom: 100 }, // x/y: offset in screen pixels from centred
    current: null,
    loggedIn: false,
    dirty: false,
    tintAuto: true                // follow the image until the user picks a colour
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
  // Downscale big uploads (max 1600px); stages are opaque, so JPEG keeps photos small.
  function normalizeSource(im) {
    var max = 1600, w = im.naturalWidth, h = im.naturalHeight;
    var c = document.createElement("canvas");
    var s = Math.min(1, max / Math.max(w, h));
    c.width = Math.max(1, Math.round(w * s)); c.height = Math.max(1, Math.round(h * s));
    var g = c.getContext("2d");
    g.fillStyle = "#fff"; g.fillRect(0, 0, c.width, c.height);
    g.drawImage(im, 0, 0, c.width, c.height);
    return c.toDataURL("image/jpeg", 0.92);
  }

  // ---------- drawing ----------
  function scale() {
    if (!state.img) return 1;
    return Math.max(W / state.img.naturalWidth, H / state.img.naturalHeight) * state.view.zoom / 100;
  }
  // Keep the image covering the whole screen.
  function clampView() {
    if (!state.img) return;
    var s = scale();
    var mx = Math.max(0, (state.img.naturalWidth * s - W) / 2), my = Math.max(0, (state.img.naturalHeight * s - H) / 2);
    state.view.x = Math.max(-mx, Math.min(mx, state.view.x));
    state.view.y = Math.max(-my, Math.min(my, state.view.y));
  }
  function drawArt(c, k) {
    c.save();
    c.scale(k, k);
    c.fillStyle = "#fff"; c.fillRect(0, 0, W, H);
    if (state.img) {
      var s = scale();
      c.imageSmoothingQuality = "high";
      c.drawImage(state.img, W / 2 + state.view.x - state.img.naturalWidth * s / 2, H / 2 + state.view.y - state.img.naturalHeight * s / 2,
        state.img.naturalWidth * s, state.img.naturalHeight * s);
    }
    c.restore();
  }
  function render() {
    drawArt(ctx, ZOOM);
    ctx.strokeStyle = "rgba(15,23,42,.25)"; ctx.lineWidth = 1;
    ctx.strokeRect(0.5, 0.5, stage.width - 1, stage.height - 1);
  }
  function artDataURL() {
    var c = document.createElement("canvas"); c.width = W; c.height = H;
    drawArt(c.getContext("2d"), 1);
    return c.toDataURL("image/jpeg", 0.95);
  }

  // Rail colour from the image: its average, a little darker so the rail reads
  // as a frame.
  function autoTint() {
    if (!state.img) return DEFAULT_TINT;
    var c = document.createElement("canvas"); c.width = 40; c.height = 24;
    var g = c.getContext("2d");
    var tmp = document.createElement("canvas"); tmp.width = W; tmp.height = H;
    drawArt(tmp.getContext("2d"), 1);
    g.drawImage(tmp, 0, 0, 40, 24);
    var d = g.getImageData(0, 0, 40, 24).data, r = 0, gr = 0, b = 0, n = d.length / 4;
    for (var i = 0; i < d.length; i += 4) { r += d[i]; gr += d[i + 1]; b += d[i + 2]; }
    var hex = function (v) { return ("0" + Math.round(v / n * 0.8).toString(16)).slice(-2); };
    return "#" + hex(r) + hex(gr) + hex(b);
  }
  function updateAutoTint() { if (state.tintAuto) $("tint").value = autoTint(); }

  // ---------- preview ----------
  var previewTimer = null, previewSeq = 0;
  function schedulePreview() { clearTimeout(previewTimer); previewTimer = setTimeout(preview, 400); }
  function preview() {
    if (!state.img) return;
    var seq = ++previewSeq;
    status($("previewStatus"), T("be.rendering", "Rendering…"));
    api("preview-stage", { method: "POST", body: { art: artDataURL(), tint: $("tint").value } }).then(function (p) {
      if (seq !== previewSeq) return;
      $("pscreen").src = p.screen; $("pstrip").src = p.strip;
      status($("previewStatus"), "");
    }).catch(function (e) { if (seq === previewSeq) status($("previewStatus"), e.message, "err"); });
  }
  function changed() { clampView(); updateAutoTint(); markDirty(); render(); schedulePreview(); }

  // ---------- input ----------
  function useFile(file) {
    if (!file) return;
    if (!/^image\/(png|jpeg|gif)$/.test(file.type)) { status($("previewStatus"), T("be.bad_type", "Please use a PNG, JPEG or GIF image."), "err"); return; }
    var reader = new FileReader();
    reader.onload = function () {
      loadImage(reader.result).then(function (im) {
        var src = normalizeSource(im);
        return loadImage(src).then(function (im2) {
          state.img = im2; state.source = src;
          state.view = { x: 0, y: 0, zoom: 100 }; $("zoom").value = 100;
          $("drophint").classList.add("hidden");
          changed();
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

  $("zoom").oninput = function () { state.view.zoom = +this.value; changed(); };
  $("fit").onclick = function () { state.view = { x: 0, y: 0, zoom: 100 }; $("zoom").value = 100; changed(); };
  stage.addEventListener("wheel", function (e) {
    if (!state.img) return;
    e.preventDefault();
    state.view.zoom = Math.max(100, Math.min(400, state.view.zoom * (e.deltaY < 0 ? 1.08 : 1 / 1.08)));
    $("zoom").value = Math.round(state.view.zoom);
    changed();
  }, { passive: false });

  var drag = null;
  function stagePoint(e) {
    var r = stage.getBoundingClientRect();
    return [(e.clientX - r.left) * W / r.width, (e.clientY - r.top) * H / r.height];
  }
  stage.addEventListener("pointerdown", function (e) {
    if (!state.img) return;
    drag = { start: stagePoint(e), x: state.view.x, y: state.view.y };
    stage.setPointerCapture(e.pointerId);
  });
  stage.addEventListener("pointermove", function (e) {
    if (!drag) return;
    var pt = stagePoint(e);
    state.view.x = drag.x + pt[0] - drag.start[0];
    state.view.y = drag.y + pt[1] - drag.start[1];
    clampView(); markDirty(); render();
  });
  stage.addEventListener("pointerup", function () { if (drag) { drag = null; updateAutoTint(); schedulePreview(); } });

  $("tint").oninput = function () { state.tintAuto = false; markDirty(); schedulePreview(); };
  $("autoTint").onclick = function () { state.tintAuto = true; updateAutoTint(); markDirty(); schedulePreview(); };

  // ---------- account & creations (shared with the badge editor) ----------
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
        return '<div class="item' + cur + '"><img' + (c.kind === "stage" || c.kind === "crane" ? ' class="wide"' : "") + ' src="' + API + "creations/" + c.id + "/art.png?v=" + encodeURIComponent(c.updatedAt) + '" alt="">' +
          '<div class="t" title="' + esc(c.title) + '">' + esc(c.title) + "</div>" +
          '<span class="pill">' + esc({ badge: T("st.tab_badge", "Badge"), stage: T("st.tab_stage", "Stage"), crane: T("ly.tab", "Layout") }[c.kind] || c.kind) + '</span> <span class="pill ' + c.status + '">' + esc(T("be.st_" + c.status, c.status)) + "</span>" +
          (c.reviewNote ? '<div class="note">' + esc(c.reviewNote) + "</div>" : "") +
          '<div class="actions"><button type="button" data-load="' + c.id + '" data-kind="' + esc(c.kind) + '">' + (c.status === "draft" || c.status === "rejected" ? T("be.edit", "Edit") : T("be.view", "View")) + "</button>" +
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
      var kind = e.target.getAttribute("data-kind");
      if (!PAGES[kind]) { alert(T("be.no_editor", "This kind of creation can't be edited any more.")); return; }
      if (kind !== "stage") { state.dirty = false; location.href = PAGES[kind] + "?id=" + id; return; }
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
      if (c.kind !== "stage") { if (PAGES[c.kind]) location.href = PAGES[c.kind] + "?id=" + id; else status($("saveStatus"), T("be.no_editor", "This kind of creation can't be edited any more."), "err"); return; }
      var spec = c.spec || {};
      return loadImage(c.source || c.art).then(function (im) {
        state.img = im; state.source = c.source || null;
        state.view = spec.view && c.source ? spec.view : { x: 0, y: 0, zoom: 100 };
        $("zoom").value = Math.round(state.view.zoom);
        state.tintAuto = false; $("tint").value = spec.tint || DEFAULT_TINT;
        $("title").value = c.title || "";
        state.current = { id: c.id, status: c.status };
        state.dirty = false;
        updateSubmit();
        $("drophint").classList.add("hidden");
        clampView(); render(); preview(); refreshList();
        status($("saveStatus"), c.status === "submitted" || c.status === "approved"
          ? F("be.locked", "This creation is {status}; “Save as copy” to change it.", { status: T("be.st_" + c.status, c.status) }) : T("be.loaded", "Loaded."));
      });
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); });
  }

  function save(asCopy) {
    if (!state.img) { status($("saveStatus"), T("be.need_image", "Add an image first."), "err"); return Promise.reject(new Error("no image")); }
    if (!$("title").value.trim()) { status($("saveStatus"), T("st.need_title", "Give the stage a title."), "err"); $("title").focus(); return Promise.reject(new Error("no title")); }
    var body = {
      kind: "stage", title: $("title").value, art: artDataURL(), source: state.source || "",
      spec: { tint: $("tint").value, view: state.view }
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
    state.img = null; state.source = null; state.current = null; state.dirty = false; state.tintAuto = true;
    state.view = { x: 0, y: 0, zoom: 100 }; $("zoom").value = 100; $("tint").value = DEFAULT_TINT;
    $("title").value = "";
    ["pscreen", "pstrip"].forEach(function (i) { $(i).removeAttribute("src"); });
    $("drophint").classList.remove("hidden");
    status($("saveStatus"), ""); status($("previewStatus"), "");
    render(); refreshList(); updateSubmit();
  };

  window.addEventListener("beforeunload", function (e) { if (state.dirty) { e.preventDefault(); e.returnValue = ""; } });

  function start() {
    render();
    refreshAccount().then(function () {
      var id = +new URLSearchParams(location.search).get("id");
      if (id && state.loggedIn) loadCreation(id);
    }).catch(function () { $("account").textContent = ""; });
  }
  if (window.I18N) I18N.ready(start); else start();
})();
