// Badge Arcade layout editor. A layout is a Nintendo template machine with its
// badge spots, obstacles and attachments rearranged (badgearcade/layout.go):
// objects sit at their centre in top-screen pixels (y down), rotate clockwise
// in degrees, badge spots scale a 128-pixel badge, parts their own texture.
// The template's joints are drawn as pins that move with their body A (the
// server keeps them the same way) and vanish with either body.
(function () {
  "use strict";
  var API = "api/";
  var SCREEN_W = 400, SCREEN_H = 240, MARGIN_X = 40, MARGIN_Y = 40, Z = 2; // canvas 960x640
  var SPOT = 128;

  var $ = function (id) { return document.getElementById(id); };
  var T = function (key, en) { return window.I18N ? I18N.t(key, en) : en; };
  var F = function (key, en, args) { var s = T(key, en); return window.I18N ? I18N.fill(s, args) : s.replace(/\{(\w+)\}/g, function (m, k) { return k in args ? args[k] : m; }); };
  var canvas = $("canvas"), ctx = canvas.getContext("2d");

  var state = {
    templates: [], obstacles: {}, attachments: {}, limits: null, palette: "obstacle",
    template: null,      // {name, stage, joints, orig: {kind: {from: object}}}
    objects: [],         // layout objects (see LayoutObject)
    sel: -1,
    images: {},          // url -> HTMLImageElement
    stageImg: null, spotImg: null,
    stages: [], badges: [], // own creations for the selects
    current: null, loggedIn: false, dirty: false
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
  function esc(s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); }
  function img(url) {
    if (!state.images[url]) {
      var im = new Image();
      im.onload = render;
      im.src = url;
      state.images[url] = im;
    }
    return state.images[url];
  }
  function partURL(kind, name) { return API + "layout/part/" + kind + "/" + encodeURIComponent(name) + ".png"; }
  function changed() { state.dirty = true; render(); updateCounts(); schedulePreview(); }

  // ---------- geometry ----------
  // size of an object at scale 1, in screen pixels
  function partInfo(o) { return (o.kind === "attachment" ? state.attachments : state.obstacles)[o.part]; }
  function baseSize(o) {
    if (o.kind === "badge") return [SPOT, SPOT];
    var ob = partInfo(o);
    return ob ? [ob.w, ob.h] : [32, 32];
  }
  // a point that moves with a body from its template pose to its current one
  function movePoint(x, y, from, to) {
    var a = from.rot * Math.PI / 180, dx = x - from.x, dy = y - from.y;
    var lx = (dx * Math.cos(a) + dy * Math.sin(a)) / from.sw * to.sw, ly = (-dx * Math.sin(a) + dy * Math.cos(a)) / from.sh * to.sh;
    var b = to.rot * Math.PI / 180;
    return [to.x + lx * Math.cos(b) - ly * Math.sin(b), to.y + lx * Math.sin(b) + ly * Math.cos(b)];
  }
  function findFrom(kind, from) {
    for (var i = 0; i < state.objects.length; i++) if (state.objects[i].kind === kind && state.objects[i].from === from) return state.objects[i];
    return null;
  }
  // screen point -> object-local point (origin at the object's centre, unscaled)
  function toLocal(o, px, py) {
    var a = o.rot * Math.PI / 180, dx = px - o.x, dy = py - o.y;
    return [(dx * Math.cos(a) + dy * Math.sin(a)) / o.sw, (-dx * Math.sin(a) + dy * Math.cos(a)) / o.sh];
  }
  function hit(o, px, py) {
    var b = baseSize(o), l = toLocal(o, px, py);
    return Math.abs(l[0]) <= b[0] / 2 && Math.abs(l[1]) <= b[1] / 2;
  }
  function canvasPoint(e) {
    var r = canvas.getBoundingClientRect();
    return [(e.clientX - r.left) * canvas.width / r.width / Z - MARGIN_X, (e.clientY - r.top) * canvas.height / r.height / Z - MARGIN_Y];
  }

  // ---------- drawing ----------
  function drawPlaced(image, w, h, x, y, sw, sh, rot, alpha) {
    ctx.save();
    ctx.globalAlpha = alpha == null ? 1 : alpha;
    ctx.translate((x + MARGIN_X) * Z, (y + MARGIN_Y) * Z);
    ctx.rotate(rot * Math.PI / 180);
    ctx.scale(sw * Z, sh * Z);
    if (image && image.complete && image.naturalWidth) ctx.drawImage(image, -w / 2, -h / 2, w, h);
    ctx.restore();
  }
  function drawOutline(o, color, polys) {
    var b = baseSize(o);
    ctx.save();
    ctx.translate((o.x + MARGIN_X) * Z, (o.y + MARGIN_Y) * Z);
    ctx.rotate(o.rot * Math.PI / 180);
    ctx.scale(o.sw * Z, o.sh * Z);
    ctx.lineWidth = 2 / Math.max(0.05, Math.min(o.sw, o.sh) * Z);
    ctx.strokeStyle = color;
    if (polys) {
      polys.forEach(function (p) {
        ctx.beginPath();
        p.forEach(function (pt, i) { (i ? ctx.lineTo : ctx.moveTo).call(ctx, pt[0] - b[0] / 2, pt[1] - b[1] / 2); });
        ctx.closePath(); ctx.stroke();
      });
    } else {
      ctx.strokeRect(-b[0] / 2, -b[1] / 2, b[0], b[1]);
    }
    ctx.restore();
  }
  function render() {
    ctx.fillStyle = "#334155"; ctx.fillRect(0, 0, canvas.width, canvas.height);
    if (!state.template) return;
    var sx = MARGIN_X * Z, sy = MARGIN_Y * Z;
    if (state.stageImg && state.stageImg.complete) ctx.drawImage(state.stageImg, sx, sy, SCREEN_W * Z, SCREEN_H * Z);
    var showCol = $("showCollision").checked;
    ["obstacle", "attachment"].forEach(function (kind) {
      state.objects.forEach(function (o) {
        if (o.kind !== kind) return;
        var ob = partInfo(o), b = baseSize(o);
        if (ob && ob.invisible) drawOutline(o, "rgba(255,255,255,.8)", ob.polys);
        else drawPlaced(img(partURL(kind === "obstacle" ? "fob" : "atb", o.part)), b[0], b[1], o.x, o.y, o.sw, o.sh, o.rot);
        if (showCol && ob) drawOutline(o, "#f0f", ob.polys);
      });
    });
    state.objects.forEach(function (o) {
      if (o.kind === "badge") drawPlaced(state.spotImg, SPOT, SPOT, o.x, o.y, o.sw, o.sh, o.rot);
    });
    // joints that still have both bodies, anchored to body A
    (state.template.joints || []).forEach(function (j) {
      var a = findFrom(j.kindA, j.a), b = findFrom(j.kindB, j.b), orig = state.template.orig[j.kindA][j.a];
      if (!a || !b || !orig) return;
      var p = movePoint(j.x, j.y, orig, a);
      ctx.beginPath(); ctx.arc((p[0] + MARGIN_X) * Z, (p[1] + MARGIN_Y) * Z, 6, 0, 2 * Math.PI);
      ctx.fillStyle = "#facc15"; ctx.fill(); ctx.lineWidth = 2; ctx.strokeStyle = "#78350f"; ctx.stroke();
    });
    // screen frame and the off-screen margin
    ctx.fillStyle = "rgba(15,23,42,.45)";
    ctx.fillRect(0, 0, canvas.width, sy); ctx.fillRect(0, sy + SCREEN_H * Z, canvas.width, canvas.height);
    ctx.fillRect(0, sy, sx, SCREEN_H * Z); ctx.fillRect(sx + SCREEN_W * Z, sy, canvas.width, SCREEN_H * Z);
    ctx.strokeStyle = "#fff"; ctx.lineWidth = 2; ctx.strokeRect(sx, sy, SCREEN_W * Z, SCREEN_H * Z);
    if (state.sel >= 0) drawOutline(state.objects[state.sel], "#facc15");
  }

  // ---------- counts / limits ----------
  function counts() {
    var c = { spots: 0, obstacles: 0, attachments: 0, obstacleParts: {}, attachmentParts: {} };
    state.objects.forEach(function (o) {
      if (o.kind === "badge") c.spots++;
      else if (o.kind === "obstacle") { c.obstacles++; c.obstacleParts[o.part] = 1; }
      else { c.attachments++; c.attachmentParts[o.part] = 1; }
    });
    return c;
  }
  // whether one more object of a kind (and part) still fits the .cib tables
  function fits(kind, part) {
    var c = counts(), L = state.limits;
    if (kind === "badge") return c.spots < L.badges;
    var n = kind === "obstacle" ? c.obstacles : c.attachments, parts = kind === "obstacle" ? c.obstacleParts : c.attachmentParts;
    var max = kind === "obstacle" ? L.obstacles : L.attachments;
    return n < max && (parts[part] || Object.keys(parts).length < max);
  }
  function updateCounts() {
    var c = counts(), L = state.limits || { badges: 20, obstacles: 20 };
    $("counts").textContent = F("ly.counts", "{spots}/{maxs} badge spots · {obs}/{maxo} obstacles", { spots: c.spots, maxs: L.badges, obs: c.obstacles, maxo: L.obstacles }) +
      " · " + F("ly.att_count", "{n}/{max} attachments", { n: c.attachments, max: L.attachments });
    $("addSpot").disabled = c.spots >= L.badges;
  }

  // ---------- selection panel ----------
  function num(v, d) { return Math.round(v * Math.pow(10, d)) / Math.pow(10, d); }
  function prop(key, label, min, max, step, value) {
    return '<div class="prop"><label>' + esc(label) + '</label><input type="range" data-k="' + key + '" min="' + min + '" max="' + max + '" step="' + step + '" value="' + value +
      '"><input type="number" data-k="' + key + '" min="' + min + '" max="' + max + '" step="' + step + '" value="' + num(value, 3) + '"></div>';
  }
  function renderProps() {
    var box = $("props");
    if (state.sel < 0) { box.innerHTML = '<p class="hint">' + esc(T("ly.nothing_selected", "Nothing selected.")) + "</p>"; return; }
    var o = state.objects[state.sel], L = state.limits;
    var h = "<p><strong>" + esc(o.kind === "badge" ? T("ly.badge_spot", "Badge spot") : o.part) + "</strong>" +
      (o.kind === "attachment" ? ' <span class="muted small">' + esc(T("ly.attachment", "attachment")) + "</span>" : "") + "</p>";
    h += prop("x", "X", -100, 500, 0.5, o.x) + prop("y", "Y", -100, 340, 0.5, o.y);
    h += prop("sw", T("ly.width", "Width"), 0.05, 3, 0.01, o.sw) + prop("sh", T("ly.height", "Height"), 0.05, 3, 0.01, o.sh);
    h += prop("rot", T("ly.rotation", "Rotation"), -180, 180, 1, o.rot);
    if (o.kind === "badge") {
      h += prop("friction", T("ly.friction", "Friction"), 0, 1, 0.01, o.friction == null ? 0.2 : o.friction);
      h += prop("bounce", T("ly.bounce", "Bounciness"), L.bounce[0], L.bounce[1], 0.01, o.bounce == null ? 0.5 : o.bounce);
      h += prop("strength", T("ly.strength", "Physics strength"), L.strength[0], L.strength[1], 0.01, o.strength == null ? 1 : o.strength);
      h += '<label class="small"><input type="checkbox" id="falls"' + (o.falls === false ? "" : " checked") + "> " + esc(T("ly.falls", "Starts falling (off: starts at rest where placed)")) + "</label>";
      h += '<p class="hint">' + esc(T("ly.physics_hint", "Friction: low slides, high sticks. Bounciness: 0 none, 1 a lot. Physics strength: low barely moves; high gets wild.")) + "</p>";
      h += '<button type="button" id="applyAll" class="small">' + esc(T("ly.apply_all", "Use these physics for all badge spots")) + "</button>";
    }
    box.innerHTML = h;
    box.querySelectorAll("input[data-k]").forEach(function (inp) {
      inp.addEventListener("input", function () {
        var v = parseFloat(this.value);
        if (isNaN(v)) return;
        o[this.getAttribute("data-k")] = v;
        box.querySelectorAll('input[data-k="' + this.getAttribute("data-k") + '"]').forEach(function (other) { if (other !== inp) other.value = num(v, 3); });
        changed();
      });
    });
    if ($("falls")) $("falls").onchange = function () { o.falls = this.checked; changed(); };
    if ($("applyAll")) $("applyAll").onclick = function () {
      state.objects.forEach(function (x) { if (x.kind === "badge") { x.friction = o.friction; x.bounce = o.bounce; x.strength = o.strength; x.falls = o.falls; } });
      changed();
    };
  }
  function select(i) { state.sel = i; renderProps(); render(); }

  // ---------- mouse ----------
  var drag = null;
  canvas.addEventListener("pointerdown", function (e) {
    if (!state.template) return;
    var p = canvasPoint(e), found = -1;
    // topmost first, in drawing order: badge spots, attachments, obstacles
    ["badge", "attachment", "obstacle"].forEach(function (kind) {
      for (var i = state.objects.length - 1; i >= 0 && found < 0; i--) {
        if (state.objects[i].kind === kind && hit(state.objects[i], p[0], p[1])) found = i;
      }
    });
    select(found);
    if (found >= 0) {
      // Bodies pinned to the dragged one (it's their joint's body A) come along,
      // as they would in the machine.
      var o = state.objects[found], carried = [];
      (state.template.joints || []).forEach(function (j) {
        if (j.kindA !== o.kind || j.a !== o.from) return;
        var b = findFrom(j.kindB, j.b);
        if (b && b !== o && carried.indexOf(b) < 0) carried.push(b);
      });
      drag = { start: p, x: o.x, y: o.y, carried: carried.map(function (b) { return { o: b, x: b.x, y: b.y }; }) };
      canvas.setPointerCapture(e.pointerId);
    }
  });
  canvas.addEventListener("pointermove", function (e) {
    if (!drag || state.sel < 0) return;
    var p = canvasPoint(e), o = state.objects[state.sel], dx = p[0] - drag.start[0], dy = p[1] - drag.start[1];
    o.x = num(drag.x + dx, 1); o.y = num(drag.y + dy, 1);
    drag.carried.forEach(function (c) { c.o.x = num(c.x + dx, 1); c.o.y = num(c.y + dy, 1); });
    render();
  });
  canvas.addEventListener("pointerup", function () { if (drag) { drag = null; renderProps(); changed(); } });
  canvas.addEventListener("wheel", function (e) {
    if (state.sel < 0) return;
    e.preventDefault();
    var o = state.objects[state.sel];
    if (e.shiftKey) o.rot = num(((o.rot + (e.deltaY < 0 ? 5 : -5)) + 540) % 360 - 180, 1);
    else { var k = e.deltaY < 0 ? 1.05 : 1 / 1.05; o.sw = num(Math.max(0.05, o.sw * k), 3); o.sh = num(Math.max(0.05, o.sh * k), 3); }
    renderProps(); changed();
  }, { passive: false });
  document.addEventListener("keydown", function (e) {
    if (state.sel < 0 || /INPUT|TEXTAREA|SELECT/.test(document.activeElement.tagName)) return;
    if (e.key === "Delete" || e.key === "Backspace") { e.preventDefault(); $("del").click(); }
  });

  // ---------- object buttons ----------
  $("addSpot").onclick = function () {
    var base = state.objects.filter(function (o) { return o.kind === "badge"; })[0];
    state.objects.push({ kind: "badge", x: 200, y: 120, sw: base ? base.sw : 0.6, sh: base ? base.sh : 0.6, rot: 0, from: -1,
      friction: base ? base.friction : 0.2, bounce: base ? base.bounce : 0.5, strength: base ? base.strength : 1, falls: base ? base.falls : true });
    select(state.objects.length - 1); changed();
  };
  $("dup").onclick = function () {
    if (state.sel < 0) return;
    var o = state.objects[state.sel];
    if (!fits(o.kind, o.part)) return;
    var copy = JSON.parse(JSON.stringify(o)); copy.x += 20; copy.y += 10; copy.from = -1;
    state.objects.push(copy); select(state.objects.length - 1); changed();
  };
  $("del").onclick = function () {
    if (state.sel < 0) return;
    state.objects.splice(state.sel, 1); select(-1); changed();
  };
  $("showCollision").onchange = render;

  // ---------- part palette (obstacles / attachments) ----------
  function renderPalette() {
    var q = $("obstacleFilter").value.toLowerCase(), lib = state.palette === "obstacle" ? state.obstacles : state.attachments;
    var names = Object.keys(lib).filter(function (n) { return n.toLowerCase().indexOf(q) >= 0; }).slice(0, 300);
    $("palette").innerHTML = names.map(function (n) {
      var ob = lib[n];
      return '<button type="button" title="' + esc(n) + '" data-part="' + esc(n) + '">' +
        (ob.invisible ? '<span class="inv">' + esc(T("ly.invisible", "invisible")) + "</span>" : '<img loading="lazy" src="' + partURL(state.palette === "obstacle" ? "fob" : "atb", n) + '" alt="">') + "</button>";
    }).join("");
  }
  function paletteTab(kind) {
    state.palette = kind;
    $("tabObstacles").classList.toggle("on", kind === "obstacle"); $("tabAttachments").classList.toggle("on", kind === "attachment");
    renderPalette();
  }
  $("tabObstacles").onclick = function (e) { e.preventDefault(); paletteTab("obstacle"); };
  $("tabAttachments").onclick = function (e) { e.preventDefault(); paletteTab("attachment"); };
  $("obstacleFilter").oninput = renderPalette;
  $("palette").addEventListener("click", function (e) {
    var b = e.target.closest("button[data-part]");
    if (!b || !state.template) return;
    var part = b.getAttribute("data-part");
    if (!fits(state.palette, part)) {
      status($("previewStatus"), T("ly.full", "This machine is full (20 of that kind)."), "err"); return;
    }
    state.objects.push({ kind: state.palette, part: part, x: 200, y: 140, sw: 1, sh: 1, rot: 0, from: -1 });
    select(state.objects.length - 1); changed();
  });

  // ---------- templates ----------
  function renderTemplates() {
    var q = $("templateFilter").value.toLowerCase();
    $("templates").innerHTML = state.templates.filter(function (t) { return t.name.toLowerCase().indexOf(q) >= 0; }).map(function (t) {
      return '<div class="tpl" data-name="' + esc(t.name) + '"><img loading="lazy" src="' + partURL("icon", t.icon) + '" alt="">' + esc(t.name) +
        "<br><span class=\"muted\">" + F("ly.tpl_info", "{spots} spots · {obs} obstacles", { spots: t.spots, obs: t.obstacles }) + "</span>" +
        (t.difficult ? '<br><span class="warn">' + esc(T("ly.difficult", "difficult")) + "</span>" : "") + "</div>";
    }).join("") || '<p class="hint">' + esc(T("ly.no_match", "No machine matches.")) + "</p>";
  }
  $("templateFilter").oninput = renderTemplates;
  function startFrom(name, blank) {
    if (state.dirty && state.template && !confirm(T("be.discard", "Discard your unsaved changes?"))) return;
    loadTemplate(name).then(function (tpl) {
      state.objects = blank ? [] : tpl.layout.objects.map(normalizeObject);
      state.current = null; state.dirty = !!blank; $("title").value = "";
      showEditor(); updateSubmit(); refreshList();
    }).catch(function (err) { alert(err.message); });
  }
  $("templates").addEventListener("click", function (e) {
    var t = e.target.closest(".tpl");
    if (t) startFrom(t.getAttribute("data-name"), false);
  });
  // Empty machines: one per crane type, on the simplest template of that type
  // (it only lends the crane, colour and icon).
  var CRANES = [[0, "ly.crane0", "Claw crane"], [1, "ly.crane1", "Hammer crane"], [3, "ly.crane3", "Stick crane"], [4, "ly.crane4", "Bomb crane"]];
  function renderBlank() {
    $("blank").innerHTML = CRANES.map(function (c) {
      var base = state.templates.filter(function (t) { return t.type === c[0] && !t.difficult; })
        .sort(function (a, b) { return a.obstacles - b.obstacles || a.name.localeCompare(b.name); })[0];
      return base ? '<button type="button" data-blank="' + esc(base.name) + '">' + esc(T(c[1], c[2])) + "</button>" : "";
    }).join("");
  }
  $("blank").addEventListener("click", function (e) {
    var b = e.target.closest("button[data-blank]");
    if (b) startFrom(b.getAttribute("data-blank"), true);
  });
  $("changeTemplate").onclick = function () { $("templateCard").hidden = false; $("templateCard").scrollIntoView({ behavior: "smooth" }); };

  function normalizeObject(o) {
    var c = { kind: o.kind, x: o.x, y: o.y, sw: o.sw, sh: o.sh, rot: o.rot, from: o.from == null ? -1 : o.from };
    if (o.kind !== "badge") c.part = o.part;
    else { c.friction = o.friction; c.bounce = o.bounce; c.strength = o.strength; c.falls = o.falls; }
    return c;
  }
  function loadTemplate(name) {
    return api("layout/template?name=" + encodeURIComponent(name)).then(function (t) {
      var orig = { badge: {}, obstacle: {}, attachment: {} };
      t.layout.objects.forEach(function (o) { orig[o.kind][o.from] = o; });
      state.template = { name: name, stage: t.stage, joints: t.joints || [], orig: orig };
      $("templateName").textContent = name;
      updateStage();
      return t;
    });
  }
  function showEditor() {
    ["editorCard", "propsCard", "previewCard", "saveCard"].forEach(function (id) { $(id).hidden = false; });
    $("templateCard").hidden = true;
    select(-1); updateCounts(); render(); preview();
  }

  // ---------- backdrop / spot badge ----------
  function updateStage() {
    var v = +$("stageSelect").value;
    state.stageImg = img(v ? API + "creations/" + v + "/art.png" : partURL("stage", state.template.stage));
    render();
  }
  function updateSpot() {
    var v = +$("badgeSelect").value;
    if (v) { state.spotImg = img(API + "creations/" + v + "/art.png"); render(); return; }
    // placeholder: a translucent disc with a ring, like the server's
    var c = document.createElement("canvas"); c.width = c.height = SPOT;
    var g = c.getContext("2d");
    g.fillStyle = "rgba(99,102,241,.9)"; g.beginPath(); g.arc(64, 64, 60, 0, 2 * Math.PI); g.fill();
    g.globalCompositeOperation = "destination-out"; g.beginPath(); g.arc(64, 64, 54, 0, 2 * Math.PI); g.fill();
    g.globalCompositeOperation = "source-over"; g.fillStyle = "rgba(255,255,255,.35)"; g.beginPath(); g.arc(64, 64, 54, 0, 2 * Math.PI); g.fill();
    var im = new Image(); im.onload = render; im.src = c.toDataURL(); state.spotImg = im;
  }
  $("stageSelect").onchange = function () { updateStage(); state.dirty = true; schedulePreview(); };
  $("badgeSelect").onchange = function () { updateSpot(); state.dirty = true; schedulePreview(); };
  function fillSelects() {
    $("stageSelect").innerHTML = '<option value="0">' + esc(T("ly.template_stage", "the machine's own stage")) + "</option>" +
      state.stages.map(function (c) { return '<option value="' + c.id + '">' + esc(c.title) + "</option>"; }).join("");
    $("badgeSelect").innerHTML = '<option value="0">' + esc(T("ly.placeholder", "placeholders")) + "</option>" +
      state.badges.map(function (c) { return '<option value="' + c.id + '">' + esc(c.title) + "</option>"; }).join("");
  }

  // ---------- spec / preview ----------
  function spec() {
    return {
      layout: { template: state.template.name, objects: state.objects },
      stage: +$("stageSelect").value || 0, badge: +$("badgeSelect").value || 0
    };
  }
  var previewTimer = null, previewSeq = 0;
  function schedulePreview() { clearTimeout(previewTimer); previewTimer = setTimeout(preview, 600); }
  function preview() {
    if (!state.template) return;
    var seq = ++previewSeq;
    if (!counts().spots) { status($("previewStatus"), T("ly.need_spot", "Add at least one badge spot."), "err"); return; }
    status($("previewStatus"), T("be.rendering", "Rendering…"));
    api("preview-layout", { method: "POST", body: spec() }).then(function (p) {
      if (seq !== previewSeq) return;
      $("pimage").src = p.image;
      status($("previewStatus"), "");
    }).catch(function (e) { if (seq === previewSeq) status($("previewStatus"), e.message, "err"); });
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
  var PAGES = { badge: "./", stage: "stage.html", crane: "layout.html" };
  var KIND_LABEL = function (k) { return { badge: T("st.tab_badge", "Badge"), stage: T("st.tab_stage", "Stage"), crane: T("ly.tab", "Layout") }[k] || k; };
  function refreshList() {
    var box = $("creations");
    if (!state.loggedIn) { box.innerHTML = '<p class="hint">' + T("be.login_to_see", "Log in to see your saved creations.") + "</p>"; fillSelects(); return Promise.resolve(); }
    return api("creations").then(function (list) {
      var keepStage = $("stageSelect").value, keepBadge = $("badgeSelect").value;
      state.stages = list.filter(function (c) { return c.kind === "stage"; });
      state.badges = list.filter(function (c) { return c.kind === "badge"; });
      fillSelects(); $("stageSelect").value = keepStage || "0"; $("badgeSelect").value = keepBadge || "0";
      if (!list.length) { box.innerHTML = '<p class="hint">' + T("be.nothing", "Nothing saved yet.") + "</p>"; return; }
      box.innerHTML = '<div class="list">' + list.map(function (c) {
        var cur = state.current && state.current.id === c.id ? " current" : "";
        return '<div class="item' + cur + '"><img' + (c.kind === "stage" || c.kind === "crane" ? ' class="wide"' : "") + ' src="' + API + "creations/" + c.id + "/art.png?v=" + encodeURIComponent(c.updatedAt) + '" alt="">' +
          '<div class="t" title="' + esc(c.title) + '">' + esc(c.title) + "</div>" +
          '<span class="pill">' + esc(KIND_LABEL(c.kind)) + '</span> <span class="pill ' + c.status + '">' + esc(T("be.st_" + c.status, c.status)) + "</span>" +
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
      if (kind !== "crane") { state.dirty = false; location.href = PAGES[kind] + "?id=" + id; return; }
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
      if (c.kind !== "crane") { if (PAGES[c.kind]) location.href = PAGES[c.kind] + "?id=" + id; else status($("saveStatus"), T("be.no_editor", "This kind of creation can't be edited any more."), "err"); return; }
      var s = c.spec || {};
      return loadTemplate(s.layout.template).then(function () {
        state.objects = (s.layout.objects || []).map(normalizeObject);
        $("stageSelect").value = String(s.stage || 0); $("badgeSelect").value = String(s.badge || 0);
        updateStage(); updateSpot();
        $("title").value = c.title || "";
        state.current = { id: c.id, status: c.status }; state.dirty = false;
        showEditor(); updateSubmit(); refreshList();
        status($("saveStatus"), c.status === "submitted" || c.status === "approved"
          ? F("be.locked", "This creation is {status}; “Save as copy” to change it.", { status: T("be.st_" + c.status, c.status) }) : T("be.loaded", "Loaded."));
      });
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); });
  }

  function save(asCopy) {
    if (!state.template) return Promise.reject(new Error("no layout"));
    if (!$("title").value.trim()) { status($("saveStatus"), T("ly.need_title", "Give the layout a title."), "err"); $("title").focus(); return Promise.reject(new Error("no title")); }
    var body = { kind: "crane", title: $("title").value, spec: spec() };
    var editable = state.current && (state.current.status === "draft" || state.current.status === "rejected");
    var req = !asCopy && editable
      ? api("creations/" + state.current.id, { method: "PUT", body: body })
      : api("creations", { method: "POST", body: body });
    status($("saveStatus"), T("be.saving", "Saving…"));
    return req.then(function (r) {
      state.current = { id: r.id, status: "draft" }; state.dirty = false;
      status($("saveStatus"), T("be.saved", "Saved."), "ok");
      refreshList(); updateSubmit();
      return r;
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); throw e; });
  }

  function updateSubmit() {
    var st = state.current && state.current.status, b = $("submit");
    b.disabled = !state.loggedIn || st === "submitted" || st === "approved";
    b.textContent = st === "submitted" ? T("be.waiting", "Waiting for review") : st === "approved" ? T("be.st_approved", "Approved") : T("be.submit_review", "Submit for review…");
  }
  $("submit").onclick = function () { if (state.template) $("rules").hidden = false; };
  $("submitCancel").onclick = function () { $("rules").hidden = true; };
  $("submitConfirm").onclick = function () {
    $("rules").hidden = true;
    var st = state.current && state.current.status;
    var saved = state.current && !state.dirty && (st === "draft" || st === "rejected") ? Promise.resolve(state.current) : save(false);
    saved.then(function (c) { return api("creations/" + c.id + "/submit", { method: "POST", body: {} }); }).then(function () {
      state.current.status = "submitted";
      status($("saveStatus"), T("be.submitted", "Submitted — you'll see the review result in “My creations”."), "ok");
      refreshList(); updateSubmit();
    }).catch(function (e) { status($("saveStatus"), e.message, "err"); });
  };
  $("save").onclick = function () { save(false).catch(function () {}); };
  $("saveCopy").onclick = function () { save(true).catch(function () {}); };
  $("new").onclick = function () {
    if (state.dirty && !confirm(T("be.discard", "Discard your unsaved changes?"))) return;
    state.template = null; state.objects = []; state.current = null; state.dirty = false;
    ["editorCard", "propsCard", "previewCard", "saveCard"].forEach(function (id) { $(id).hidden = true; });
    $("templateCard").hidden = false; $("title").value = "";
    status($("saveStatus"), ""); updateSubmit(); refreshList();
  };
  window.addEventListener("beforeunload", function (e) { if (state.dirty) { e.preventDefault(); e.returnValue = ""; } });

  function start() {
    updateSpot(); render();
    Promise.all([api("layout/templates"), api("layout/obstacles")]).then(function (r) {
      state.templates = r[0];
      r[1].obstacles.forEach(function (o) { state.obstacles[o.name] = o; });
      (r[1].attachments || []).forEach(function (o) { state.attachments[o.name] = o; });
      state.limits = r[1].limits;
      renderTemplates(); renderBlank(); renderPalette();
    }).catch(function (e) { $("templates").innerHTML = '<p class="status err">' + esc(e.message) + "</p>"; });
    refreshAccount().then(function () {
      var id = +new URLSearchParams(location.search).get("id");
      if (id && state.loggedIn) loadCreation(id);
    }).catch(function () { $("account").textContent = ""; });
  }
  if (window.I18N) I18N.ready(start); else start();
})();
