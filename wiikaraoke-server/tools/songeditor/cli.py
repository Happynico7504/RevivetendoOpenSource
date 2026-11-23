"""
CLI entrypoint the web editor's backend (relay-admin, Go) calls as a
subprocess — keeps the actual format logic in Python (where it's proven
correct) without porting it. Reads a JSON spec from stdin, writes the built
.ujk to the path the spec names, and prints one JSON line to stdout:
  {"ok": true, "total_ms": ..., "note_count": ...}
  {"ok": false, "error": "..."}
Exit code is 0 on success, 1 on any failure (including a bad spec).

Spec fields:
  rpx_path          path to the game dump's code/app.rpx (server-controlled)
  template_path     a real on-disc .ujk to borrow palette/records/font/tail
                     from (server-controlled)
  notes_mode        "manual" | "midi" | "acapella"
  manual_notes      for notes_mode="manual": list of [duration_ms, pitch]
                     (pitch: int, note-name string, or null for a rest)
  aux_path          for notes_mode="midi"/"acapella": the .mid file or the
                     isolated vocal/melody recording to pitch-track
  audio_path        this song's own backing track (any ffmpeg-readable
                     format) — gets re-encoded to AAC/ADTS and trimmed to
                     match the authored notes' total length
  event3_ms         optional, default 1000 (ms before the first note)
  preamble_overrides optional {event_index: duration_ms} for any of the
                     preamble's 8 events (0-7) - shrinks the intro below
                     what event3_ms alone can reach; see song.Song
  mirror_real       optional bool - see song.Song's docstring
  title/artist/writer/composer
                     optional - generates a title-card PNG (titlecard.py)
                     instead of borrowing the template's; see song.Song.build
  lyrics_mode       optional: "manual" | "lrc" | "acapella" - selects
                     which of the fields below produces lyrics_lines; see
                     lyrics_sync.py. Omit entirely for no custom lyrics.
  lyrics_lines      for lyrics_mode="manual": list of [text, start_ms,
                     end_ms] (absolute track time, same zero point as
                     total_ms)
  lyrics_lrc        for lyrics_mode="lrc": the raw .lrc file content (a
                     string, not a path - the editor UI reads the upload
                     client-side and sends its text)
  lyrics_raw_lines  for lyrics_mode="acapella": plain lyric lines, no
                     timestamps, in singing order. The actual ML sync
                     against a vocal recording does NOT happen here - it
                     only ever runs through the background queue
                     (relay-admin/songeditor_lyrics_sync.go +
                     lyrics_sync_cli.py), since it's a ~25-30s CPU-heavy
                     job that needs throttling this synchronous, 60s-
                     timeout endpoint cannot provide. This endpoint just
                     consumes whatever that queue already wrote back to
                     lyrics_lines - raises an error if that's missing.
  out_path          where to write the finished .ujk
"""
import json
import os
import sys
import tempfile

from . import audio, container, gamedata, song, sszl, track
from . import lyrics_sync
from . import notes as notes_mod

AUDIO_SAFETY_MARGIN_MS = 3000


def _build(spec):
    rpx_path = spec["rpx_path"]
    template_path = spec["template_path"]
    xk = gamedata.xor_table(rpx_path)
    key = gamedata.blowfish_key(rpx_path)
    real = container.read_real(template_path, key, xk)
    real_table = container.read_table(real, xk)
    joy_plain = sszl.decompress(container.read_section(real, real_table, 2))
    joy = track.parse_joy(joy_plain)
    real_track_events = track.parse_events(joy.tv_track)

    event3_ms = spec.get("event3_ms", 1000)
    # lead_in_ms: the TOTAL desired preamble length (all 8 events
    # proportionally scaled to it - track.scale_preamble_overrides),
    # not just event3_ms's own single-event override. Priority: an
    # explicit preamble_overrides always wins outright (full manual
    # control); otherwise an explicit lead_in_ms (set by the player, or
    # cached back by notes_sync_cli.py/lyrics_sync_cli.py's own
    # auto-detection - see notes.detect_lead_in_ms) is used as the scale
    # target; otherwise, if an aux recording is directly available at
    # this point (notes_mode="midi"/"acapella", both fast/inline), it's
    # auto-detected right here. Confirmed 2026-11-14 this matters: the
    # TEMPLATE's own preamble length has no relationship to any given
    # recording's real lead-in (one real case: 11376ms of template
    # preamble vs ~7.9s of actual silence before singing starts),
    # causing note/lyric extraction to start already mid-phrase.
    preamble_overrides = spec.get("preamble_overrides")
    lead_in_ms = None
    if preamble_overrides:
        preamble_overrides = {int(k): v for k, v in preamble_overrides.items()}
    else:
        lead_in_ms = spec.get("lead_in_ms")
        if lead_in_ms is None and spec.get("aux_path") and spec.get("notes_mode") == "acapella":
            lead_in_ms = notes_mod.detect_lead_in_ms(spec["aux_path"])
        if lead_in_ms is not None:
            preamble_overrides = track.scale_preamble_overrides(real_track_events, lead_in_ms, event3_ms=event3_ms)
    preamble, _rest = track.extract_preamble(real_track_events, event3_ms, preamble_overrides)
    preamble_ms = track.preamble_duration_ms(preamble)

    mode = spec["notes_mode"]
    if mode == "manual":
        note_list = notes_mod.from_manual(spec["manual_notes"])
    elif mode == "midi":
        note_list = notes_mod.from_midi(spec["aux_path"])
    elif mode == "acapella":
        # The recording's own leading silence/intro must not also be
        # counted by the synthetic preamble Song.build() prepends below -
        # see from_acapella's skip_ms docstring.
        note_list = notes_mod.from_acapella(spec["aux_path"], skip_ms=preamble_ms)
    elif mode == "acapella_ml":
        # notes.from_acapella_ml (torchcrepe) only ever runs through the
        # background queue (relay-admin/songeditor_notes_sync.go +
        # notes_sync_cli.py) - confirmed 2026-11-14 it takes ~55-66s per
        # song, far too slow/heavy for this synchronous, 60s-timeout
        # endpoint. This just consumes whatever that queue already wrote
        # back to manual_notes, exactly like "manual" mode - the player
        # must run the separate sync job first.
        if not spec.get("manual_notes"):
            raise ValueError("run the notes sync job first (see the Sync notes button), then build")
        note_list = notes_mod.from_manual(spec["manual_notes"])
    else:
        raise ValueError("unknown notes_mode %r" % (mode,))
    if not note_list:
        raise ValueError("no notes were produced")
    # Applied regardless of notes_mode/source - see cap_rest_length's own
    # docstring: a single rest far longer than anything real discs use
    # risks track.mirror_rest() hitting an untested edge case, confirmed
    # 2026-11-15 as the likely cause of a hang on real hardware.
    note_list = notes_mod.cap_rest_length(note_list)

    mirror_real = spec.get("mirror_real", False)

    # lyrics_mode's three input modes all produce timestamps relative to
    # the SONG'S OWN AUDIO (what a player editing against a waveform/
    # video would naturally enter, or what an .lrc file's own tags
    # already mean) - shifted here by the preamble's own duration so
    # they land correctly on song.Song's "absolute track time, same zero
    # point as total_ms" timeline, which starts before the preamble.
    # Same principle as notes_mod.from_acapella's own skip_ms handling.
    lyrics_mode = spec.get("lyrics_mode")
    lyrics_lines = None
    if lyrics_mode == "manual":
        lyrics_lines = lyrics_sync.from_manual(spec["lyrics_lines"])
        lyrics_lines = [(t, s + preamble_ms, e + preamble_ms) for t, s, e in lyrics_lines]
    elif lyrics_mode == "lrc":
        lyrics_lines = lyrics_sync.from_lrc(spec["lyrics_lrc"])
        lyrics_lines = [(t, s + preamble_ms, e + preamble_ms) for t, s, e in lyrics_lines]
    elif lyrics_mode == "acapella":
        # The ML sync itself only ever runs through the background queue
        # (relay-admin/songeditor_lyrics_sync.go + lyrics_sync_cli.py) -
        # it's a ~25-30s CPU-heavy job that must be throttled to one at a
        # time at the lowest priority, which this synchronous build
        # endpoint (60s timeout, runs inline on the request) cannot do
        # safely. The regular build just consumes whatever the queue
        # already wrote back to spec.lyrics_lines, exactly like "manual"
        # mode - the player must run the separate sync job first.
        if not spec.get("lyrics_lines"):
            raise ValueError("run the lyrics sync job first (see the Sync lyrics button), then build")
        lyrics_lines = lyrics_sync.from_manual(spec["lyrics_lines"])
        lyrics_lines = [(t, s + preamble_ms, e + preamble_ms) for t, s, e in lyrics_lines]
    elif lyrics_mode is not None:
        raise ValueError("unknown lyrics_mode %r" % (lyrics_mode,))

    if lyrics_lines is not None:
        # Removes drift between the notes and lyrics pipelines at its
        # source - see lyrics_sync.snap_to_notes's own docstring. Applied
        # regardless of lyrics_mode, since the drift it fixes comes from
        # comparing two independently-timed sources, not from any one
        # input mode's own math.
        lyrics_lines = lyrics_sync.snap_to_notes(lyrics_lines, note_list, preamble_ms)
        # Applied after snapping, which could reintroduce the same
        # ordering problem - see enforce_min_display's own docstring.
        lyrics_lines = lyrics_sync.enforce_min_display(lyrics_lines)

    s = song.Song(note_list, event3_ms=event3_ms, preamble_overrides=preamble_overrides,
                  mirror_real=mirror_real, lyrics_lines=lyrics_lines)
    total_ms = s.total_ms(real_track_events)

    adts_path = spec["audio_path"]
    tmp_adts = None
    raw = open(adts_path, "rb").read()
    if not (len(raw) >= 2 and raw[0] == 0xFF and (raw[1] & 0xF0) == 0xF0):
        fd, tmp_adts = tempfile.mkstemp(suffix=".adts")
        os.close(fd)
        audio.encode_to_adts(adts_path, tmp_adts)
        raw = open(tmp_adts, "rb").read()
    try:
        # Ask for a few seconds more audio than the notes strictly need.
        # Trimming exactly flush to the last note's end leaves no slack
        # between where the track stops and where the decoded audio
        # actually runs out - a long, dense track built this way left only
        # ~2ms of margin and froze the game. trim_to_frame_boundary caps
        # gracefully at the source's real length if it's shorter than this,
        # so asking for extra margin here is always safe.
        trimmed, actual_ms = audio.trim_to_frame_boundary(raw, total_ms + AUDIO_SAFETY_MARGIN_MS)
        if actual_ms < total_ms * 0.95:
            raise ValueError(
                "backing track (%.1fs) is too short for the authored notes (%.1fs)"
                % (actual_ms / 1000, total_ms / 1000)
            )
        s.build(rpx_path, template_path, trimmed, spec["out_path"],
                title=spec.get("title"), artist=spec.get("artist"),
                writer=spec.get("writer"), composer=spec.get("composer"))
    finally:
        if tmp_adts:
            os.remove(tmp_adts)

    result = {
        "total_ms": total_ms,
        "actual_audio_ms": actual_ms,
        "note_count": len(note_list),
        # the extracted (duration_ms, pitch) list, same shape as manual_notes -
        # lets the editor show midi/acapella results in manual mode for touch-up
        "notes": note_list,
    }
    if lead_in_ms is not None:
        # cached back the same way manual_notes/lyrics_lines are, so a
        # later notes/lyrics sync job (or another "acapella" build) picks
        # up this build's own auto-detected value instead of redetecting -
        # and so the UI can show/let the player touch it up.
        result["lead_in_ms"] = lead_in_ms
    if lyrics_lines is not None:
        # shifted back to audio-relative ms (undoing the preamble_ms
        # shift above) - same convention as lyrics_mode="manual"'s own
        # input, so the editor can show an lrc/acapella result in manual
        # mode for touch-up, exactly like notes/manual_notes above.
        result["lyrics_lines"] = [(t, round(s - preamble_ms), round(e - preamble_ms))
                                   for t, s, e in lyrics_lines]
    return result


def main():
    spec = json.load(sys.stdin)
    try:
        result = _build(spec)
        print(json.dumps({"ok": True, **result}))
    except Exception as e:  # noqa: BLE001 - reported to the caller, not re-raised
        print(json.dumps({"ok": False, "error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
