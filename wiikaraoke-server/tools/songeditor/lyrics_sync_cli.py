"""
CLI entrypoint for the background lyrics-sync worker (relay-admin's Go
queue, songeditor_lyrics_sync.go) - kept separate from cli.py's full song
build since syncing lyrics against an a cappella recording doesn't need
a template/audio-trimming/font-generation context, only the raw lines
and the recording - but it DOES need the same preamble-duration offset
cli.py applies to every other lyrics mode (see song.Song's lyrics_lines
docstring), so this still reads the template to get that one number.

Reads JSON from stdin:
  {"rpx_path": ..., "template_path": ..., "event3_ms": 1000,
   "preamble_overrides": {...}, "raw_lines": [...], "aux_path": "...",
   "use_ml": true}
Writes one JSON line to stdout:
  {"ok": true, "lyrics_lines": [[text, start_ms, end_ms], ...]}
  {"ok": false, "error": "..."}
Exit code 0 on success, 1 on any failure - same convention as cli.py.
"""
import json
import sys

from . import container, gamedata, lyrics_sync, notes as notes_mod, sszl, track


def _build(spec):
    xk = gamedata.xor_table(spec["rpx_path"])
    key = gamedata.blowfish_key(spec["rpx_path"])
    real = container.read_real(spec["template_path"], key, xk)
    real_table = container.read_table(real, xk)
    joy_plain = sszl.decompress(container.read_section(real, real_table, 2))
    joy = track.parse_joy(joy_plain)
    real_track_events = track.parse_events(joy.tv_track)

    event3_ms = spec.get("event3_ms", 1000)
    # Same priority order as cli.py/notes_sync_cli.py: explicit
    # preamble_overrides wins outright; otherwise an explicit lead_in_ms
    # (whether set by the player, or already cached back by whichever
    # of the two sync workers happened to run first) is the scale
    # target; otherwise auto-detect from THIS aux recording (lyrics has
    # its own, separate upload slot from notes', but both are isolated
    # vocal recordings of the same song, so either one's detected
    # lead-in is equally valid as the shared preamble target).
    preamble_overrides = spec.get("preamble_overrides")
    lead_in_ms = spec.get("lead_in_ms")
    if preamble_overrides:
        preamble_overrides = {int(k): v for k, v in preamble_overrides.items()}
    else:
        if lead_in_ms is None:
            lead_in_ms = notes_mod.detect_lead_in_ms(spec["aux_path"])
        preamble_overrides = track.scale_preamble_overrides(real_track_events, lead_in_ms, event3_ms=event3_ms)
    preamble, _rest = track.extract_preamble(real_track_events, event3_ms, preamble_overrides)
    preamble_ms = track.preamble_duration_ms(preamble)

    lines = spec["raw_lines"]
    if not lines:
        raise ValueError("no lyric lines given")
    fn = lyrics_sync.from_acapella if spec.get("use_ml", True) else lyrics_sync.from_acapella_vad
    result = fn(lines, spec["aux_path"], skip_ms=preamble_ms)
    # from_acapella/from_acapella_vad return TRACK-absolute timestamps
    # (they add skip_ms internally), but spec.lyrics_lines is always
    # stored audio-relative - the same convention cli.py's own "manual"
    # build path reads back (it adds preamble_ms on the way in and
    # subtracts it on the way out) - so this worker's output must match
    # that storage convention, or the main build would double-add the
    # preamble offset once it reads spec.lyrics_lines back.
    lyrics_lines = [(t, s - preamble_ms, e - preamble_ms) for t, s, e in result]
    return lyrics_lines, lead_in_ms


def main():
    spec = json.load(sys.stdin)
    try:
        lyrics_lines, lead_in_ms = _build(spec)
        print(json.dumps({"ok": True, "lyrics_lines": lyrics_lines, "lead_in_ms": lead_in_ms}))
    except Exception as e:  # noqa: BLE001 - reported to the caller, not re-raised
        print(json.dumps({"ok": False, "error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
