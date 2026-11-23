"""
CLI entrypoint for the background notes-sync worker (relay-admin's Go
queue, songeditor_notes_sync.go) - mirrors lyrics_sync_cli.py exactly,
kept as a fully separate, parallel pipeline per the user's explicit
choice 2026-11-14 (a generalized single queue was offered and declined
in favor of keeping the two concerns independent).

notes.from_acapella_ml (torchcrepe, a neural CPU pitch detector) is
meaningfully more accurate than notes.from_acapella's autocorrelation
heuristic, but confirmed 2026-11-14 to take ~55-66s and ~560MB peak RSS
for a 3.5-minute song - too slow/heavy to run inline on a request the
same way the old fast autocorrelation path still does, so this only
ever runs through the background queue, same reasoning as lyrics'
own ML sync.

Reads JSON from stdin:
  {"rpx_path": ..., "template_path": ..., "event3_ms": 1000,
   "preamble_overrides": {...}, "aux_path": "..."}
Writes one JSON line to stdout:
  {"ok": true, "notes": [[duration_ms, pitch], ...]}
  {"ok": false, "error": "..."}
Exit code 0 on success, 1 on any failure - same convention as cli.py.
"""
import json
import sys

from . import container, gamedata, notes as notes_mod, sszl, track


def _build(spec):
    xk = gamedata.xor_table(spec["rpx_path"])
    key = gamedata.blowfish_key(spec["rpx_path"])
    real = container.read_real(spec["template_path"], key, xk)
    real_table = container.read_table(real, xk)
    joy_plain = sszl.decompress(container.read_section(real, real_table, 2))
    joy = track.parse_joy(joy_plain)
    real_track_events = track.parse_events(joy.tv_track)

    event3_ms = spec.get("event3_ms", 1000)
    # Mirrors cli.py's own priority order (explicit preamble_overrides >
    # explicit lead_in_ms > auto-detected from this aux recording) - see
    # cli.py's _build for why: the TEMPLATE's own preamble length has no
    # relationship to this recording's real lead-in, so skip_ms must be
    # based on THIS recording, not left at the template's own value.
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

    notes = notes_mod.from_acapella_ml(spec["aux_path"], skip_ms=preamble_ms)
    return notes, lead_in_ms


def main():
    spec = json.load(sys.stdin)
    try:
        notes, lead_in_ms = _build(spec)
        print(json.dumps({"ok": True, "notes": notes, "lead_in_ms": lead_in_ms}))
    except Exception as e:  # noqa: BLE001 - reported to the caller, not re-raised
        print(json.dumps({"ok": False, "error": str(e)}))
        sys.exit(1)


if __name__ == "__main__":
    main()
