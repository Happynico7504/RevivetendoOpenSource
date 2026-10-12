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
.dl{display:flex;gap:.4rem;align-items:center;flex-wrap:wrap;margin-top:.45rem;font-size:.85rem}
.dl select{width:auto;padding:.25rem .4rem;font-size:.85rem}
.dl input{width:5rem;border:1px solid #d1d5db;border-radius:6px;padding:.25rem .4rem;font:inherit;font-size:.85rem}
.dl button{background:#2563eb;color:#fff;border:none;border-radius:6px;padding:.3rem .7rem;font:inherit;font-size:.85rem;cursor:pointer}
.dl .ok{color:#166534}.dl .err{color:#991b1b}
.chan{border:1px solid #e4e4e7;border-radius:10px;padding:.75rem;margin-bottom:.75rem}
.chan .pics{display:flex;gap:.75rem;align-items:flex-end;flex-wrap:wrap;margin-bottom:.5rem}
.chan .pics img{border-radius:4px;background:#f1f5f9;max-width:100%;height:auto}
.chan h3{font-size:1rem;margin:0 0 .25rem;word-break:break-word}
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
  <li data-i18n="vid.rule_download">You can let people download your videos to their 3DS, for free or for RevivetendoCoins that go to you. Downloadable videos get their own channel in the eShop.</li>
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
  <div id="chanpick" style="display:none">
  <label for="channel" data-i18n="vid.f_channel">Channel</label>
  <select id="channel"><option value="0" data-i18n="vid.channel_staff">Revivetendo TV (staff choose the channel)</option></select>
  </div>
  <label class="check"><input id="rights" type="checkbox" required> <span data-i18n="vid.f_rights">I made this video or have the right to share it, and it follows the rules above.</span></label>
  <button class="submit" id="go" type="submit" data-i18n="vid.f_upload">Upload</button>
  <div class="bar" id="bar"><div></div></div>
  <div class="msg" id="msg"></div>
</form>

<h2 data-i18n="vid.yours">Your videos</h2>
<div id="list"><p class="empty" data-i18n="vid.loading">Loading…</p></div>

<h2 data-i18n="vid.ch_h2">Your channels</h2>
<p class="intro" data-i18n="vid.ch_intro">Want your own channel in the eShop? Submit a name, an icon and a banner. Once staff approve it, you can pick it for your uploads.</p>
<div id="chlist"></div>
<form id="chform">
  <label for="chname" data-i18n="vid.ch_f_name">Channel name</label>
  <input id="chname" type="text" maxlength="60" required>
  <label for="chdesc" data-i18n="vid.f_desc">Description (optional)</label>
  <textarea id="chdesc" maxlength="500"></textarea>
  <label for="chicon" data-i18n="vid.ch_f_icon">Icon</label>
  <input id="chicon" type="file" accept="image/jpeg,image/png,image/webp" required>
  <p class="hint" data-i18n="vid.ch_icon_hint">Shown at 102×76 — it's cropped to fit.</p>
  <label for="chbanner" data-i18n="vid.ch_f_banner">Banner</label>
  <input id="chbanner" type="file" accept="image/jpeg,image/png,image/webp" required>
  <p class="hint" data-i18n="vid.ch_banner_hint">Shown at 400×168 — it's cropped to fit.</p>
  <label class="check"><input id="chrights" type="checkbox" required> <span data-i18n="vid.ch_f_rights">I made these images or have the right to use them, and they follow the rules above.</span></label>
  <button class="submit" id="chgo" type="submit" data-i18n="vid.ch_f_submit">Submit channel for review</button>
  <div class="msg" id="chmsg"></div>
</form>

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
  var CHSTATUS = {
    submitted: ['vid.st_submitted', 'Waiting for review'],
    live: ['vid.ch_st_live', 'Live in the eShop'],
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

  var pollTimer = null, hasWallet = false;

  // downloadRow is the per-video download option (off / free / paid).
  function downloadRow(v) {
    var row = el('div', 'dl');
    row.appendChild(el('span', '', t('vid.dl_label', 'Download to 3DS:')));
    var sel = el('select');
    [['off', 'vid.dl_off', 'Off'], ['free', 'vid.dl_free', 'Free'], ['paid', 'vid.dl_paid', 'Paid']].forEach(function (o) {
      var opt = el('option', '', t(o[1], o[2])); opt.value = o[0];
      if (o[0] === 'paid' && !hasWallet) opt.disabled = true;
      sel.appendChild(opt);
    });
    sel.value = v.download || 'off';
    var price = el('input'); price.type = 'number'; price.min = 1; price.max = 500; price.value = v.price || 1;
    var coins = el('span', '', t('vid.dl_coins', 'coins'));
    var save = el('button', '', t('vid.dl_save', 'Save'));
    var note = el('span');
    function sync() { var paid = sel.value === 'paid'; price.style.display = coins.style.display = paid ? '' : 'none'; }
    sel.onchange = sync; sync();
    save.onclick = function () {
      note.className = ''; note.textContent = '';
      api('POST', 'download', { id: v.id, mode: sel.value, price: +price.value }).then(function (r) {
        if (r.ok) { note.className = 'ok'; note.textContent = t('vid.dl_saved', 'Saved.'); }
        else { note.className = 'err'; note.textContent = apiErr(r); }
      });
    };
    [sel, price, coins, save, note].forEach(function (e) { row.appendChild(e); });
    if (!hasWallet) row.appendChild(el('div', 'hint', t('vid.dl_no_wallet', 'To charge coins you need a RevivetendoCoin wallet: open the eShop on your 3DS once.')));
    return row;
  }

  function load() {
    api('GET', 'list').then(function (j) {
      var list = $('list'), vids = j.videos || [], busy = false;
      hasWallet = !!j.hasWallet;
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
        var label = t(st[0], st[1]);
        if (v.status === 'processing' && v.queueAhead === 0) label = t('vid.queue_next', 'Waiting to convert — you\'re next');
        else if (v.status === 'processing' && v.queueAhead > 0) label = fmt(t('vid.queue_ahead', 'Waiting to convert — {n} ahead of you'), { n: v.queueAhead });
        info.appendChild(el('span', 'st st-' + v.status, label));
        var meta = v.created + (v.seconds ? ' · ' + mmss(v.seconds) : '') + (v.stereo ? ' · 3D' : '');
        if (v.status === 'uploading') meta += ' · ' + Math.floor(100 * v.received / v.size) + '%';
        info.appendChild(el('div', 'meta', meta));
        if (v.status === 'live') info.appendChild(el('div', 'meta', fmt(t('vid.stats', '{views} views · {likes} likes · {comments} comments'), { views: v.views, likes: v.likes, comments: v.comments })));
        if (v.reviewNote) info.appendChild(el('div', 'note', v.reviewNote));
        if (v.status === 'submitted' || v.status === 'live' || v.status === 'rejected') info.appendChild(downloadRow(v));
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
      showChannels(j.channels || []);
      clearTimeout(pollTimer);
      if (busy) pollTimer = setTimeout(load, 5000);
    });
  }

  function showChannels(chans) {
    var list = $('chlist'), pick = $('channel'), keep = pick.value;
    list.innerHTML = '';
    while (pick.options.length > 1) pick.remove(1);
    chans.forEach(function (c) {
      if (c.status === 'live') { var o = el('option', '', c.name); o.value = c.catalogId; pick.appendChild(o); }
      var box = el('div', 'chan'), pics = el('div', 'pics');
      ['icon', 'banner'].forEach(function (k) {
        var img = el('img'); img.src = '/inkay/my/videos/channel-media/' + c.id + '/' + k + '.jpg'; img.alt = '';
        img.width = k === 'icon' ? 102 : 400; img.height = k === 'icon' ? 76 : 168; pics.appendChild(img);
      });
      box.appendChild(pics);
      box.appendChild(el('h3', '', c.name));
      var st = CHSTATUS[c.status] || [c.status, c.status];
      box.appendChild(el('span', 'st st-' + c.status, t(st[0], st[1])));
      box.appendChild(el('div', 'meta', c.created));
      if (c.description) box.appendChild(el('div', 'meta', c.description));
      if (c.reviewNote) box.appendChild(el('div', 'note', c.reviewNote));
      var acts = el('div', 'acts');
      var del = el('button', 'del', t('vid.delete', 'Delete'));
      del.onclick = function () {
        if (!confirm(t('vid.ch_confirm_delete', 'Delete this channel? Your videos in it move to the main channel.'))) return;
        api('POST', 'channel-delete', { id: c.id }).then(function (r) { if (r.ok) load(); else alert(apiErr(r)); });
      };
      acts.appendChild(del);
      box.appendChild(acts);
      list.appendChild(box);
    });
    pick.value = keep;
    if (pick.selectedIndex < 0) pick.value = '0';
    $('chanpick').style.display = pick.options.length > 1 ? '' : 'none';
  }

  $('chform').onsubmit = function (ev) {
    ev.preventDefault();
    var m = $('chmsg'), fd = new FormData();
    fd.append('name', $('chname').value);
    fd.append('description', $('chdesc').value);
    fd.append('icon', $('chicon').files[0]);
    fd.append('banner', $('chbanner').files[0]);
    if ($('chrights').checked) fd.append('rights', '1');
    $('chgo').disabled = true;
    m.className = 'msg'; m.textContent = t('vid.ch_sending', 'Sending…');
    fetch('/inkay/my/videos/api/channel', { method: 'POST', credentials: 'same-origin', headers: { 'X-Requested-With': 'revivetendo' }, body: fd })
      .then(function (r) { return r.json().catch(function () { return { error: r.status === 413 ? t('vid.ch_err_image_size', 'Images can be at most 4 MB each.') : '' }; }); })
      .then(function (j) {
        if (!j.id) throw j;
        m.className = 'msg ok'; m.textContent = t('vid.ch_sent', 'Submitted! Staff will review your channel.');
        $('chform').reset();
        load();
      }, function () { throw { error: t('vid.err_network', 'Connection lost.') }; })
      .catch(function (j) { m.className = 'msg err'; m.textContent = apiErr(j || {}); })
      .then(function () { $('chgo').disabled = false; });
  };

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
    api('POST', 'start', { title: $('title').value, description: $('desc').value, size: file.size, rights: $('rights').checked, stereo: $('stereo').value, channel: +$('channel').value }).then(function (j) {
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
