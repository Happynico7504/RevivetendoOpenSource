// Wii Karaoke U song editor. All URLs are relative: the public site sits
// behind a path prefix, and the page lives under /my/ so the login cookie
// reaches the API (see songeditor.go).
(function () {
  "use strict";
  var API = "api/"; // page is /inkay/my/songeditor/ on the public site

  var $ = function (id) { return document.getElementById(id); };
  var esc = function (s) { return String(s).replace(/[&<>"]/g, function (c) { return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]; }); };

  var state = { loggedIn: false, current: null, audioBlobURL: null, lyricsLrcText: null, lyricsPollTimer: null, notesPollTimer: null };

  function api(path, opts) {
    opts = opts || {};
    if (opts.body && typeof opts.body !== "string" && !(opts.body instanceof FormData)) {
      opts.body = JSON.stringify(opts.body);
      opts.headers = { "Content-Type": "application/json" };
    }
    opts.credentials = "same-origin";
    return fetch(API + path, opts).then(function (r) {
      if (opts._raw) return r;
      return r.json().catch(function () { return {}; }).then(function (j) {
        if (!r.ok) throw new Error(j.error || ("HTTP " + r.status));
        return j;
      });
    });
  }

  function show(id, on) { $(id).classList.toggle("hidden", !on); }

  // ---------- list view ----------
  function renderList(items) {
    var ul = $("songList");
    ul.innerHTML = "";
    items.forEach(function (it) {
      var li = document.createElement("li");
      li.innerHTML =
        '<span>#' + it.id + " " + esc(it.title || "(untitled)") + "</span>" +
        '<span class="pill ' + it.status + '">' + it.status + "</span>";
      li.addEventListener("click", function () { openSong(it.id); });
      ul.appendChild(li);
    });
    if (!items.length) {
      var li = document.createElement("li");
      li.textContent = "No songs yet — click “+ New song” to start.";
      li.style.cursor = "default";
      ul.appendChild(li);
    }
  }

  function refreshList() {
    return api("creations").then(renderList);
  }

  // ---------- edit view ----------
  function notesModeHint(mode) {
    if (mode === "midi") return "Upload a .mid file. The track with the most notes is used, reduced to one voice.";
    if (mode === "acapella") return "Upload an isolated vocal/melody recording (not the full mix) — pitch is tracked automatically. Best-effort: check the result, touch up in manual mode if needed.";
    if (mode === "acapella_ml") return "Upload an isolated vocal/melody recording (not the full mix) — used by the Sync notes button below.";
    return "";
  }

  function updateAuxVisibility() {
    // the aux upload is shared between notes_mode's own midi/acapella/
    // acapella_ml upload and lyrics_mode="acapella" (voice-activity
    // doesn't care about pitch, so the same file works for both) - show
    // it if ANY of those need it, not just notes mode.
    show("auxBlock", currentNotesMode() !== "manual" || currentLyricsMode() === "acapella");
  }

  function notesSyncLabel(s) {
    if (s.notesSyncStatus === "processing" && s.notesQueueAhead === 0) return "Syncing now… (usually takes a minute or so)";
    if (s.notesSyncStatus === "processing") return "Syncing…";
    if (s.notesSyncStatus === "queued" && s.notesQueueAhead === 0) return "Waiting to sync — you're next";
    if (s.notesSyncStatus === "queued") return "Waiting to sync — " + s.notesQueueAhead + " ahead of you";
    if (s.notesSyncStatus === "done") return "Synced — switch to Manual mode to view or touch it up.";
    if (s.notesSyncStatus === "failed") return "Sync failed: " + s.notesSyncError;
    return "";
  }

  function pollNotesSync(id) {
    clearTimeout(state.notesPollTimer);
    api("creations/" + id).then(function (s) {
      if (state.current && state.current.id === id) {
        state.current = s;
        $("notesSyncStatus").textContent = notesSyncLabel(s);
        if (s.notesSyncStatus === "done") {
          $("manualNotes").value = formatManualNotes(s.spec && s.spec.manual_notes);
          if ($("leadInInput").value.trim() === "" && s.spec && s.spec.lead_in_ms != null) {
            $("leadInInput").value = s.spec.lead_in_ms;
          }
        }
      }
      if (s.notesSyncStatus === "queued" || s.notesSyncStatus === "processing") {
        state.notesPollTimer = setTimeout(function () { pollNotesSync(id); }, 5000);
      }
    });
  }

  function setNotesMode(mode) {
    document.querySelectorAll('input[name="notesMode"]').forEach(function (r) { r.checked = r.value === mode; });
    show("manualBlock", mode === "manual");
    show("notesSyncBlock", mode === "acapella_ml");
    $("auxHint").textContent = notesModeHint(mode);
    updateAuxVisibility();
  }

  function currentNotesMode() {
    var checked = document.querySelector('input[name="notesMode"]:checked');
    return checked ? checked.value : "manual";
  }

  // ---------- lyrics ----------
  function setLyricsMode(mode) {
    document.querySelectorAll('input[name="lyricsMode"]').forEach(function (r) { r.checked = r.value === mode; });
    show("lyricsManualBlock", mode === "manual");
    show("lyricsLrcBlock", mode === "lrc");
    show("lyricsAcapellaBlock", mode === "acapella");
    updateAuxVisibility();
  }

  function currentLyricsMode() {
    var checked = document.querySelector('input[name="lyricsMode"]:checked');
    return checked ? checked.value : "none";
  }

  function lyricsSyncLabel(s) {
    if (s.lyricsSyncStatus === "processing" && s.lyricsQueueAhead === 0) return "Syncing now…";
    if (s.lyricsSyncStatus === "processing") return "Syncing…";
    if (s.lyricsSyncStatus === "queued" && s.lyricsQueueAhead === 0) return "Waiting to sync — you're next";
    if (s.lyricsSyncStatus === "queued") return "Waiting to sync — " + s.lyricsQueueAhead + " ahead of you";
    if (s.lyricsSyncStatus === "done") return "Synced — switch to Manual lyrics mode to view or touch it up.";
    if (s.lyricsSyncStatus === "failed") return "Sync failed: " + s.lyricsSyncError;
    return "";
  }

  function pollLyricsSync(id) {
    clearTimeout(state.lyricsPollTimer);
    api("creations/" + id).then(function (s) {
      if (state.current && state.current.id === id) {
        state.current = s;
        $("lyricsSyncStatus").textContent = lyricsSyncLabel(s);
        if (s.lyricsSyncStatus === "done") {
          $("lyricsManual").value = formatLyricsManual(s.spec && s.spec.lyrics_lines);
          if ($("leadInInput").value.trim() === "" && s.spec && s.spec.lead_in_ms != null) {
            $("leadInInput").value = s.spec.lead_in_ms;
          }
        }
      }
      if (s.lyricsSyncStatus === "queued" || s.lyricsSyncStatus === "processing") {
        state.lyricsPollTimer = setTimeout(function () { pollLyricsSync(id); }, 5000);
      }
    });
  }

  function parseLyricsManual(text) {
    var out = [];
    text.split("\n").forEach(function (line) {
      line = line.trim();
      if (!line) return;
      var parts = line.split(",");
      if (parts.length < 3) throw new Error('bad lyrics line: "' + line + '" (expected "start_ms, end_ms, text")');
      var startMs = parseInt(parts[0], 10);
      var endMs = parseInt(parts[1], 10);
      if (isNaN(startMs) || isNaN(endMs)) throw new Error('bad lyrics line: "' + line + '" (start/end must be numbers)');
      var lineText = parts.slice(2).join(",").trim();
      out.push([lineText, startMs, endMs]);
    });
    return out;
  }

  function formatLyricsManual(lines) {
    return (lines || []).map(function (l) { return l[1] + ", " + l[2] + ", " + l[0]; }).join("\n");
  }

  function parseManualNotes(text) {
    var out = [];
    text.split("\n").forEach(function (line) {
      line = line.trim();
      if (!line) return;
      var parts = line.split(",");
      var ms = parseInt(parts[0], 10);
      if (isNaN(ms)) throw new Error('bad line: "' + line + '" (expected "duration_ms, pitch")');
      var pitchStr = (parts[1] || "").trim();
      var pitch = null;
      if (pitchStr !== "") {
        pitch = /^-?\d+$/.test(pitchStr) ? parseInt(pitchStr, 10) : pitchStr;
      }
      out.push([ms, pitch]);
    });
    return out;
  }

  function formatManualNotes(notes) {
    return (notes || []).map(function (n) { return n[0] + (n[1] === null || n[1] === undefined ? "," : ", " + n[1]); }).join("\n");
  }

  function clearEditResult() { $("buildResult").textContent = ""; $("buildResult").className = ""; }

  function openSong(id) {
    api("creations/" + id).then(function (s) {
      state.current = s;
      show("listView", false);
      show("editView", true);
      clearEditResult();
      $("titleInput").value = s.title || "";
      $("artistInput").value = (s.spec && s.spec.artist) || "";
      $("writerInput").value = (s.spec && s.spec.writer) || "";
      $("composerInput").value = (s.spec && s.spec.composer) || "";
      $("event3Input").value = s.spec && s.spec.event3_ms ? s.spec.event3_ms : 1000;
      $("leadInInput").value = (s.spec && s.spec.lead_in_ms != null) ? s.spec.lead_in_ms : "";
      var mode = (s.spec && s.spec.notes_mode) || "manual";
      setNotesMode(mode);
      $("manualNotes").value = formatManualNotes(s.spec && s.spec.manual_notes);
      $("notesSyncStatus").textContent = notesSyncLabel(s);
      clearTimeout(state.notesPollTimer);
      if (s.notesSyncStatus === "queued" || s.notesSyncStatus === "processing") {
        state.notesPollTimer = setTimeout(function () { pollNotesSync(id); }, 5000);
      }

      var lyricsMode = (s.spec && s.spec.lyrics_mode) || "none";
      setLyricsMode(lyricsMode);
      $("lyricsManual").value = formatLyricsManual(s.spec && s.spec.lyrics_lines);
      $("lyricsRawLines").value = ((s.spec && s.spec.lyrics_raw_lines) || []).join("\n");
      state.lyricsLrcText = (s.spec && s.spec.lyrics_lrc) || null;
      $("lyricsLrcName").textContent = state.lyricsLrcText ? "a .lrc file is saved (upload again to replace)" : "no file uploaded yet";
      $("lyricsAuxName").textContent = s.hasLyricsAux ? s.lyricsAuxName + " (uploaded)" : "no file uploaded yet";
      $("lyricsSyncStatus").textContent = lyricsSyncLabel(s);
      clearTimeout(state.lyricsPollTimer);
      if (s.lyricsSyncStatus === "queued" || s.lyricsSyncStatus === "processing") {
        state.lyricsPollTimer = setTimeout(function () { pollLyricsSync(id); }, 5000);
      }

      $("audioName").textContent = s.hasAudio ? s.audioName + " (uploaded)" : "no file uploaded yet";
      $("auxName").textContent = s.hasAux ? s.auxName + " (uploaded)" : "no file uploaded yet";
      if (state.audioBlobURL) { URL.revokeObjectURL(state.audioBlobURL); state.audioBlobURL = null; }
      if (s.hasAudio) {
        $("audioPreview").src = API + "creations/" + id + "/audio";
        show("audioPreview", true);
      } else {
        show("audioPreview", false);
      }
      var editable = s.status === "draft" || s.status === "rejected";
      $("saveBtn").disabled = !editable;
      $("buildBtn").disabled = !editable;
      $("submitBtn").classList.toggle("hidden", s.status !== "draft" && s.status !== "rejected");
      $("withdrawBtn").classList.toggle("hidden", s.status !== "submitted");
      $("deleteBtn").disabled = s.status === "approved";
      if (s.hasBuilt) {
        $("downloadLink").href = API + "creations/" + id + "/download";
        show("downloadLink", true);
      } else {
        show("downloadLink", false);
      }
      $("editStatus").textContent = s.status + (s.buildError ? " — build error: " + s.buildError : "");
      $("editStatus").className = "status" + (s.buildError ? " err" : "");
    });
  }

  function currentSpec() {
    var mode = currentNotesMode();
    var spec = { notes_mode: mode, event3_ms: parseInt($("event3Input").value, 10) || 1000 };
    var leadInRaw = $("leadInInput").value.trim();
    if (leadInRaw !== "") {
      spec.lead_in_ms = parseInt(leadInRaw, 10) || 0;
    } else if (state.current && state.current.spec && state.current.spec.lead_in_ms != null) {
      // Blank means "use auto-detect", but an already-cached auto-detected
      // value (from an earlier sync/build) must still round-trip through a
      // plain Save, same reasoning as acapella_ml's manual_notes above -
      // otherwise a Save with no sync re-run would silently lose it.
      spec.lead_in_ms = state.current.spec.lead_in_ms;
    }
    if (mode === "manual") {
      spec.manual_notes = parseManualNotes($("manualNotes").value);
    } else if (mode === "acapella_ml") {
      // The background sync worker writes its result straight into the
      // database, not through this save path - a plain Save still sends
      // the whole spec, so without re-including it, a save would lose
      // an earlier sync's result. The Build button also needs it
      // present (cli.py refuses to build acapella_ml without it).
      if (state.current && state.current.spec && state.current.spec.manual_notes) {
        spec.manual_notes = state.current.spec.manual_notes;
      }
    }

    var title = $("titleInput").value.trim();
    if (title) spec.title = title;
    var artist = $("artistInput").value.trim();
    if (artist) spec.artist = artist;
    var writer = $("writerInput").value.trim();
    if (writer) spec.writer = writer;
    var composer = $("composerInput").value.trim();
    if (composer) spec.composer = composer;

    var lyricsMode = currentLyricsMode();
    if (lyricsMode !== "none") {
      spec.lyrics_mode = lyricsMode;
      if (lyricsMode === "manual") {
        spec.lyrics_lines = parseLyricsManual($("lyricsManual").value);
      } else if (lyricsMode === "lrc") {
        if (!state.lyricsLrcText) throw new Error("upload a .lrc file before saving/building lyrics mode \"lrc\"");
        spec.lyrics_lrc = state.lyricsLrcText;
      } else if (lyricsMode === "acapella") {
        var rawLines = $("lyricsRawLines").value.split("\n").map(function (l) { return l.trim(); }).filter(Boolean);
        if (!rawLines.length) throw new Error("paste at least one lyric line for a cappella sync");
        spec.lyrics_raw_lines = rawLines;
        // The background sync worker writes its result straight into the
        // database (jsonb_set), not through this save path - but a plain
        // Save still sends the WHOLE spec object, so without re-including
        // whatever's already there, it would overwrite (lose) an earlier
        // sync's result. The Build button also needs it present (see
        // cli.py's lyrics_mode="acapella" handling - it refuses to build
        // without an already-synced lyrics_lines, on purpose).
        if (state.current && state.current.spec && state.current.spec.lyrics_lines) {
          spec.lyrics_lines = state.current.spec.lyrics_lines;
        }
      }
    }
    return spec;
  }

  function saveSong() {
    var spec;
    try { spec = currentSpec(); } catch (e) { alert(e.message); return Promise.reject(e); }
    return api("creations/" + state.current.id, {
      method: "PUT",
      body: { title: $("titleInput").value, spec: spec },
    });
  }

  function uploadFile(which, file) {
    var fd = new FormData();
    fd.append("file", file);
    return api("creations/" + state.current.id + "/" + which, { method: "POST", body: fd });
  }

  // ---------- wiring ----------
  document.addEventListener("DOMContentLoaded", function () {
    document.querySelectorAll('input[name="notesMode"]').forEach(function (r) {
      r.addEventListener("change", function () { setNotesMode(r.value); });
    });

    document.querySelectorAll('input[name="lyricsMode"]').forEach(function (r) {
      r.addEventListener("change", function () { setLyricsMode(r.value); });
    });

    $("lyricsLrcFile").addEventListener("change", function () {
      var f = $("lyricsLrcFile").files[0];
      if (!f) return;
      var reader = new FileReader();
      reader.onload = function () {
        state.lyricsLrcText = reader.result;
        $("lyricsLrcName").textContent = f.name + " (loaded, not yet saved)";
      };
      reader.onerror = function () { $("lyricsLrcName").textContent = "could not read " + f.name; };
      reader.readAsText(f);
    });

    $("newSongBtn").addEventListener("click", function () {
      api("creations", { method: "POST" }).then(function (r) { refreshList().then(function () { openSong(r.id); }); });
    });

    $("backBtn").addEventListener("click", function () {
      show("editView", false);
      show("listView", true);
      refreshList();
    });

    $("audioFile").addEventListener("change", function () {
      var f = $("audioFile").files[0];
      if (!f) return;
      $("audioName").textContent = "uploading " + f.name + "…";
      saveSong().then(function () { return uploadFile("audio", f); }).then(function () {
        $("audioName").textContent = f.name + " (uploaded)";
        openSong(state.current.id);
      }).catch(function (e) { $("audioName").textContent = "upload failed: " + e.message; });
    });

    $("auxFile").addEventListener("change", function () {
      var f = $("auxFile").files[0];
      if (!f) return;
      $("auxName").textContent = "uploading " + f.name + "…";
      saveSong().then(function () { return uploadFile("aux", f); }).then(function () {
        $("auxName").textContent = f.name + " (uploaded)";
      }).catch(function (e) { $("auxName").textContent = "upload failed: " + e.message; });
    });

    $("lyricsAuxFile").addEventListener("change", function () {
      var f = $("lyricsAuxFile").files[0];
      if (!f) return;
      $("lyricsAuxName").textContent = "uploading " + f.name + "…";
      saveSong().then(function () { return uploadFile("lyricsAux", f); }).then(function () {
        $("lyricsAuxName").textContent = f.name + " (uploaded)";
      }).catch(function (e) { $("lyricsAuxName").textContent = "upload failed: " + e.message; });
    });

    $("lyricsSyncBtn").addEventListener("click", function () {
      $("lyricsSyncStatus").textContent = "Queuing…";
      saveSong()
        .then(function () { return api("creations/" + state.current.id + "/lyrics-sync", { method: "POST", body: {} }); })
        .then(function () { pollLyricsSync(state.current.id); })
        .catch(function (e) { $("lyricsSyncStatus").textContent = "Could not start sync: " + e.message; });
    });

    $("notesSyncBtn").addEventListener("click", function () {
      $("notesSyncStatus").textContent = "Queuing…";
      saveSong()
        .then(function () { return api("creations/" + state.current.id + "/notes-sync", { method: "POST", body: {} }); })
        .then(function () { pollNotesSync(state.current.id); })
        .catch(function (e) { $("notesSyncStatus").textContent = "Could not start sync: " + e.message; });
    });

    $("saveBtn").addEventListener("click", function () {
      saveSong().then(function () { openSong(state.current.id); }).catch(function (e) { alert(e.message); });
    });

    $("buildBtn").addEventListener("click", function () {
      var result = $("buildResult");
      result.textContent = "Building…";
      result.className = "";
      saveSong()
        .then(function () { return api("creations/" + state.current.id + "/build", { method: "POST", body: {} }); })
        .then(function (r) {
          var msg = "Built: " + r.noteCount + " notes, " + Math.round(r.totalMS) + "ms total, audio trimmed to " + Math.round(r.actualAudioMS) + "ms.";
          if (currentNotesMode() !== "manual" && r.notes) {
            msg += " The extracted notes were saved to Manual mode — switch to it above to view or touch them up.";
          }
          // "acapella" lyrics mode no longer builds inline (see cli.py's
          // lyrics_mode="acapella" handling) - only "lrc" resolves its
          // lines synchronously as part of a regular build.
          if (currentLyricsMode() === "lrc" && r.lyricsLines && r.lyricsLines.length) {
            msg += " The synced lyric line timing was saved to Manual lyrics mode — switch to it above to view or touch it up.";
          }
          result.textContent = msg;
          result.className = "ok";
          openSong(state.current.id);
        })
        .catch(function (e) {
          result.textContent = "Build failed: " + e.message;
          result.className = "err";
          openSong(state.current.id);
        });
    });

    $("submitBtn").addEventListener("click", function () {
      api("creations/" + state.current.id + "/submit", { method: "POST", body: {} })
        .then(function () { openSong(state.current.id); })
        .catch(function (e) { alert(e.message); });
    });

    $("withdrawBtn").addEventListener("click", function () {
      api("creations/" + state.current.id + "/withdraw", { method: "POST", body: {} })
        .then(function () { openSong(state.current.id); })
        .catch(function (e) { alert(e.message); });
    });

    $("deleteBtn").addEventListener("click", function () {
      if (!confirm("Delete this song for good?")) return;
      api("creations/" + state.current.id, { method: "DELETE" }).then(function () {
        show("editView", false);
        show("listView", true);
        refreshList();
      });
    });

    api("me").then(function (me) {
      state.loggedIn = me.loggedIn;
      show("loggedOut", !me.loggedIn);
      show("loggedIn", me.loggedIn);
      $("who").textContent = me.loggedIn ? "logged in as " + me.pnid : "";
      if (me.loggedIn) refreshList();
    });
  });
})();
