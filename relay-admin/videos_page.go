package main

import (
	"html/template"
	"net/http"
)

// myVideosPage is the upload page (videos.go has the API it drives).
func myVideosPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/my/videos" && r.URL.Path != "/my/videos/" {
		http.NotFound(w, r)
		return
	}
	if _, ok := mySessionPID(r); !ok {
		http.Redirect(w, r, "/inkay/my/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	w.Header().Set("Cache-Control", "no-store")
	myVideosTmpl.Execute(w, map[string]any{"ChunkSize": videoChunkSize, "MaxUpload": videoMaxUpload})
}

var myVideosTmpl = template.Must(template.New("my-videos").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
` + i18nScript + `
<meta charset="utf-8">
<title>My Videos — Revivetendo TV</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;max-width:640px;margin:2rem auto;padding:0 1rem;color:#222}
h1{font-size:1.3rem;margin-bottom:.5rem}
h2{font-size:1.05rem;margin:2rem 0 .75rem}
.nav{display:flex;align-items:center;gap:.5rem;margin-bottom:1.5rem;font-size:.875rem}
.nav a{color:#2563eb}
form.logout-form{display:inline;margin:0}
button.logout-btn{background:none;border:none;color:#dc2626;font-size:.875rem;cursor:pointer;padding:0;text-decoration:underline}
.intro{color:#444;font-size:.9rem;line-height:1.45}
.rules{background:#f8fafc;border:1px solid #e2e8f0;border-radius:8px;padding:.6rem 1rem .6rem 2rem;font-size:.85rem;color:#444}
.rules li{margin:.2rem 0}
label{display:block;font-size:.8rem;font-weight:600;color:#555;margin:1rem 0 .3rem}
input[type=text],textarea{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:6px;padding:.5rem .75rem;font:inherit}
textarea{min-height:4.5rem;resize:vertical}
input[type=file]{font:inherit;font-size:.9rem}
select{width:100%;box-sizing:border-box;border:1px solid #d1d5db;border-radius:6px;padding:.45rem .6rem;font:inherit;background:#fff}
.hint{font-size:.8rem;color:#666;margin:.3rem 0 0}
label.check{display:flex;gap:.5rem;align-items:flex-start;font-weight:400;color:#333;font-size:.875rem}
button.submit{margin-top:1.25rem;width:100%;background:#e2231a;color:#fff;border:none;border-radius:6px;padding:.65rem;font:inherit;cursor:pointer}
button.submit:disabled{opacity:.6;cursor:default}
.bar{height:8px;background:#e5e7eb;border-radius:4px;overflow:hidden;margin-top:1rem;display:none}
.bar div{height:100%;width:0;background:#e2231a;transition:width .2s}
.msg{font-size:.875rem;margin-top:.75rem;min-height:1.2em}
.msg.err{color:#991b1b}.msg.ok{color:#166534}
.vid{display:grid;grid-template-columns:120px 1fr;gap:1rem;border:1px solid #e4e4e7;border-radius:10px;padding:.75rem;margin-bottom:.75rem}
.vid img{width:120px;height:88px;border-radius:6px;background:#f1f5f9;object-fit:cover}
.vid .ph{width:120px;height:88px;border-radius:6px;background:#f1f5f9}
.vid h3{font-size:1rem;margin:0 0 .25rem;word-break:break-word}
.st{display:inline-block;font-size:.75rem;border-radius:999px;padding:.1rem .6rem;margin-bottom:.35rem}
.st-uploading,.st-processing{background:#e0e7ff;color:#3730a3}.st-submitted{background:#fef9c3;color:#854d0e}
.st-live{background:#dcfce7;color:#166534}.st-rejected,.st-failed{background:#fee2e2;color:#991b1b}
.meta{font-size:.8rem;color:#666}
.note{font-size:.85rem;background:#fff7ed;border:1px solid #fed7aa;border-radius:6px;padding:.35rem .6rem;margin-top:.4rem;white-space:pre-wrap}
.acts{margin-top:.4rem;font-size:.85rem}
.acts a,.acts button{color:#2563eb;background:none;border:none;padding:0;font:inherit;cursor:pointer;text-decoration:underline;margin-right:.75rem}
.acts button.del{color:#dc2626}
.empty{color:#666;font-size:.9rem}
</style>
</head>
<body data-i18n-page-title="vid.title">
<div class="nav" data-lang-slot>
  <a href="/inkay/my/" data-i18n="nav.back_my">← Back to My Status</a>
  <span style="color:#d1d5db">|</span>
  <form class="logout-form" method="post" action="/inkay/my/logout">
    <button class="logout-btn" type="submit" data-i18n="nav.signout">Sign out</button>
  </form>
</div>
<h1 data-i18n="vid.h1">My Videos</h1>
<p class="intro" data-i18n="vid.intro">Upload a video for Revivetendo TV — the video channel in the Nintendo eShop on Wii U and 3DS. Staff review every video before it goes live.</p>
<ul class="rules">
  <li data-i18n="vid.rule_size">Up to 300 MB and 10 minutes. MP4 works best.</li>
  <li data-i18n="vid.rule_rights">Only upload videos you made yourself or have the right to share.</li>
  <li data-i18n="vid.rule_content">Nothing offensive, nothing that breaks the law, no personal information.</li>
  <li data-i18n="vid.rule_screen">The 3DS shows videos at 400×240, so big text and simple shots look best.</li>
</ul>

<form id="up">
  <label for="title" data-i18n="vid.f_title">Title</label>
  <input id="title" type="text" maxlength="60" required>
  <label for="desc" data-i18n="vid.f_desc">Description (optional)</label>
  <textarea id="desc" maxlength="500"></textarea>
  <label for="file" data-i18n="vid.f_file">Video file</label>
  <input id="file" type="file" accept="video/*" required>
  <label for="stereo" data-i18n="vid.f_3d">3D video</label>
  <select id="stereo">
    <option value="" data-i18n="vid.3d_no">No, it's a normal 2D video</option>
    <option value="sbs" data-i18n="vid.3d_sbs">Yes: side by side, left eye on the left</option>
    <option value="sbs-r" data-i18n="vid.3d_sbsr">Yes: side by side, right eye on the left</option>
    <option value="tb" data-i18n="vid.3d_tb">Yes: top/bottom, left eye on top</option>
    <option value="tb-r" data-i18n="vid.3d_tbr">Yes: top/bottom, right eye on top</option>
  </select>
  <p class="hint" data-i18n="vid.3d_hint">3D videos play in 3D on the 3DS. The Wii U shows the left eye.</p>
  <p class="hint" data-i18n="vid.3d_mv">Spatial videos from an iPhone or VR headset (MV-HEVC) are detected automatically — just upload them.</p>
  <label class="check"><input id="rights" type="checkbox" required> <span data-i18n="vid.f_rights">I made this video or have the right to share it, and it follows the rules above.</span></label>
  <button class="submit" id="go" type="submit" data-i18n="vid.f_upload">Upload</button>
  <div class="bar" id="bar"><div></div></div>
  <div class="msg" id="msg"></div>
</form>

<h2 data-i18n="vid.yours">Your videos</h2>
<div id="list"><p class="empty" data-i18n="vid.loading">Loading…</p></div>

<script>
(function () {
  var CHUNK = {{.ChunkSize}}, MAX = {{.MaxUpload}};
  var STATUS = {
    uploading: ['vid.st_uploading', 'Upload not finished'],
    processing: ['vid.st_processing', 'Converting…'],
    failed: ['vid.st_failed', 'Conversion failed'],
    submitted: ['vid.st_submitted', 'Waiting for review'],
    live: ['vid.st_live', 'Live on Revivetendo TV'],
    rejected: ['vid.st_rejected', 'Rejected']
  };
  function t(k, f) { return window.I18N ? I18N.t(k, f) : f; }
  function fmt(s, a) { return s.replace(/\{(\w+)\}/g, function (m, k) { return k in a ? String(a[k]) : m; }); }
  function $(id) { return document.getElementById(id); }
  function api(method, url, body) {
    return fetch('/inkay/my/videos/api/' + url, {
      method: method, credentials: 'same-origin',
      headers: { 'X-Requested-With': 'revivetendo', 'Content-Type': body instanceof Blob ? 'application/octet-stream' : 'application/json' },
      body: body === undefined ? undefined : (body instanceof Blob ? body : JSON.stringify(body))
    }).then(function (r) { return r.json().catch(function () { return {}; }).then(function (j) { j._status = r.status; return j; }); });
  }
  function msg(text, cls) { var m = $('msg'); m.textContent = text; m.className = 'msg ' + (cls || ''); }
  function apiErr(j) { return j.key ? t(j.key, j.error) : (j.error || t('vid.err_generic', 'Something went wrong, please try again.')); }
  function el(tag, cls, text) { var e = document.createElement(tag); if (cls) e.className = cls; if (text !== undefined) e.textContent = text; return e; }
  function mmss(s) { return Math.floor(s / 60) + ':' + ('0' + (s % 60)).slice(-2); }

  var pollTimer = null;
  function load() {
    api('GET', 'list').then(function (j) {
      var list = $('list'), vids = j.videos || [], busy = false;
      list.innerHTML = '';
      if (!vids.length) { list.appendChild(el('p', 'empty', t('vid.none', 'You haven\'t uploaded any videos yet.'))); }
      vids.forEach(function (v) {
        busy = busy || v.status === 'processing';
        var box = el('div', 'vid');
        if (v.hasPreview) { var img = el('img'); img.src = '/inkay/my/videos/media/' + v.id + '/thumb.jpg'; img.alt = ''; box.appendChild(img); }
        else box.appendChild(el('div', 'ph'));
        var info = el('div');
        info.appendChild(el('h3', '', v.title));
        var st = STATUS[v.status] || [v.status, v.status];
        info.appendChild(el('span', 'st st-' + v.status, t(st[0], st[1])));
        var meta = v.created + (v.seconds ? ' · ' + mmss(v.seconds) : '') + (v.stereo ? ' · 3D' : '');
        if (v.status === 'uploading') meta += ' · ' + Math.floor(100 * v.received / v.size) + '%';
        info.appendChild(el('div', 'meta', meta));
        if (v.status === 'live') info.appendChild(el('div', 'meta', fmt(t('vid.stats', '{views} views · {likes} likes · {comments} comments'), { views: v.views, likes: v.likes, comments: v.comments })));
        if (v.reviewNote) info.appendChild(el('div', 'note', v.reviewNote));
        if (v.status === 'failed') info.appendChild(el('div', 'note', t(v.errorKey || 'vid.failed_help', v.error || 'This video could not be converted. Try another file (MP4 works best).')));
        var acts = el('div', 'acts');
        if (v.hasPreview) { var a = el('a', '', t('vid.watch', 'Watch preview')); a.href = '/inkay/my/videos/media/' + v.id + '/video.mp4'; a.target = '_blank'; acts.appendChild(a); }
        if (v.status !== 'processing') {
          var del = el('button', 'del', t(v.status === 'live' ? 'vid.remove_live' : 'vid.delete', v.status === 'live' ? 'Remove from Revivetendo TV' : 'Delete'));
          del.onclick = function () {
            if (!confirm(t('vid.confirm_delete', 'Delete this video? This can\'t be undone.'))) return;
            api('POST', 'delete', { id: v.id }).then(function (r) { if (r.ok) load(); else alert(apiErr(r)); });
          };
          acts.appendChild(del);
        }
        info.appendChild(acts);
        box.appendChild(info);
        list.appendChild(box);
      });
      clearTimeout(pollTimer);
      if (busy) pollTimer = setTimeout(load, 5000);
    });
  }

  function setProgress(done, total) {
    $('bar').style.display = 'block';
    $('bar').firstChild.style.width = (total ? 100 * done / total : 0) + '%';
  }

  // sendFrom sends the file in CHUNK-sized pieces starting at offset, retrying
  // a failed piece a few times and resyncing to the server's offset on 409.
  function sendFrom(id, file, offset, tries) {
    if (offset >= file.size) return Promise.resolve();
    setProgress(offset, file.size);
    return api('PUT', 'chunk?id=' + id + '&offset=' + offset, file.slice(offset, offset + CHUNK)).then(function (j) {
      if (j._status === 200) return j.done ? Promise.resolve() : sendFrom(id, file, j.received, 0);
      if (j._status === 409 && typeof j.received === 'number') return sendFrom(id, file, j.received, tries);
      throw j;
    }, function (e) { throw { error: t('vid.err_network', 'Connection lost.'), key: 'vid.err_network', retry: true }; }).catch(function (j) {
      if (tries < 4 && (j.retry || j._status >= 500)) {
        return new Promise(function (ok) { setTimeout(ok, 2000 * (tries + 1)); }).then(function () { return sendFrom(id, file, offset, tries + 1); });
      }
      throw j;
    });
  }

  $('up').onsubmit = function (ev) {
    ev.preventDefault();
    var file = $('file').files[0];
    if (!file) return;
    if (file.size > MAX) { msg(t('vid.err_size', 'Videos can be at most 300 MB.'), 'err'); return; }
    $('go').disabled = true;
    msg(t('vid.uploading', 'Uploading… keep this page open.'));
    api('POST', 'start', { title: $('title').value, description: $('desc').value, size: file.size, rights: $('rights').checked, stereo: $('stereo').value }).then(function (j) {
      if (!j.id) throw j;
      load();
      return sendFrom(j.id, file, 0, 0);
    }).then(function () {
      setProgress(1, 1);
      msg(t('vid.uploaded', 'Uploaded! It\'s being converted now and then goes to review.'), 'ok');
      $('up').reset();
      load();
    }).catch(function (j) {
      msg(apiErr(j || {}), 'err');
      load();
    }).then(function () { $('go').disabled = false; });
  };

  if (window.I18N) I18N.ready(load); else load();
})();
</script>
</body>
</html>`))
