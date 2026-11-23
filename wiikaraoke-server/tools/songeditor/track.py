"""
JOY-U1 blob and its track event format.

JOY-U1 blob (after SSZL decompression): b"JOY-U1" + 6 x u32 BE offsets,
splitting the rest into 7 sections: info, palette (fixed boilerplate,
copy verbatim from any real song — see project_wii_karaoke.md), TV
records, TV track, GamePad records, GamePad track, tail. The TV/GamePad
copies are normally identical content.

Track event: [varint duration_ms][u8 d1][u8 d2][d1-1 more "payload" bytes].
`d1` is a byte count (1-4 seen so far), `d2` is effectively the opcode.
The first 8 events of every real song's track are a fixed system preamble
(same (d1,d2) pairs, same order, across every real song) — any custom
track must start with a real preamble borrowed from a template song,
only overriding event index 3's duration (see project_wk_track_timing_sync.md).

Melody/vocal guide notes (project_wk_melody_vocal_opcodes.md, confirmed by
statistically decoding a real track rather than guessing): d1=3, d2=0x80
is a melody-guide note (duration = note length, payload = 2-byte little-
endian MIDI pitch number); d2=0x88 is the paired vocal-guide marker, same
pitch, duration 0. This module's `note_pair()` is the only note-authoring
API — nothing else in this package should hand-build (d1, d2, payload)
tuples for notes.

NOT implemented yet: lyric-sweep timing (opcode 0xE3 and friends) and the
scrolling pitch-corridor (0x0C/0x0D). Songs built with this package show
no lyrics of their own yet — see Song.build in song.py.
"""
import struct
from collections import namedtuple

Event = namedtuple("Event", "duration_ms d1 d2 payload")

PREAMBLE_LEN = 8
MELODY_OPCODE = 0x80
MELODY_ADVANCE_OPCODE = 0x81  # fires once per melody note in real tracks - see note_pair()
VOCAL_OPCODE = 0x88
VOCAL_ADVANCE_OPCODE = 0x89  # same companion relationship as 0x81 has to 0x80, weaker signal
REST_OPCODE = 0xE3  # seen throughout real tracks' bodies between notes; zero-payload like c0/c1
LINE_START_OPCODE = 0xC0  # show-lyric-line marker, zero payload
LINE_END_OPCODE = 0xC1  # hide-lyric-line marker, zero payload
CORRIDOR_OPCODE = 0x0D  # scrolling pitch-corridor visual position - see corridor_event() below
CORRIDOR_REBASE_OPCODE = 0x0C  # same payload shape as 0x0D, much rarer in real tracks (~1 per 9)

# The on-screen note display's own payload is a single byte, always one of
# these 8-9 discrete levels evenly spread across 0-255 (confirmed 2026-11-14
# across 10 real disc songs - see project_wk_melody_vocal_opcodes.md). The
# real formula is still unconfirmed (it's not a static pitch->lane mapping -
# the value keeps changing across several 0x0D events even while the same
# note is still held, so it's something closer to an animated cursor than a
# direct per-note value); this is a best-effort approximation (linearly
# mapping the note's own pitch into a fixed assumed vocal range) good enough
# to stop the renderer's "no corridor data at all" collapse into one long
# note, not a byte-exact reproduction of the real encoding.
_CORRIDOR_LANES = [0, 32, 63, 95, 126, 158, 190, 221, 253]
_CORRIDOR_PITCH_RANGE = (48, 84)  # assumed typical vocal range, MIDI


def encode_varint(v):
    if v == 0:
        return bytes([0])
    groups = []
    while v > 0:
        groups.append(v & 0x7F)
        v >>= 7
    groups.reverse()
    out = bytearray()
    for i, g in enumerate(groups):
        out.append(g | 0x80 if i < len(groups) - 1 else g)
    return bytes(out)


def _read_varint(data, p):
    varint = 0
    while True:
        b = data[p]
        p += 1
        varint = (varint << 7) | (b & 0x7F)
        if b < 0x80:
            break
    return varint, p


def parse_events(data):
    events = []
    p = 0
    n = len(data)
    while p < n:
        duration, p = _read_varint(data, p)
        d1 = data[p]
        p += 1
        d2 = data[p]
        p += 1
        payload = data[p:p + d1 - 1]
        p += d1 - 1
        events.append(Event(duration, d1, d2, payload))
    assert p == n, "track did not parse to an exact end (%d of %d bytes)" % (p, n)
    return events


def interleave_absolute(relative_events, absolute_events):
    """Merge a list of Events using the format's usual self-duration
    encoding (each event's own duration_ms is how long IT lasts before
    the next one fires - confirmed by Song.total_ms()'s own plain sum of
    note durations matching real audio length throughout this session)
    with a list of (abs_ms, Event) tuples given in ABSOLUTE track time
    (e.g. lyrics.show_hide_events' output). Returns a single Event list
    back in the same self-duration encoding, in time order. Ties keep
    relative_events before absolute_events at the same timestamp
    (arbitrary but deterministic) rather than depending on Python's sort
    stability alone, since merging two differently-sourced streams by
    time is exactly where an off-by-one ordering bug would hide."""
    merged = []
    t = 0
    fire_times = set()
    for e in relative_events:
        merged.append((t, 0, e))
        fire_times.add(t)
        t += e.duration_ms
    # Nudge any absolute event that would otherwise land at the EXACT
    # same millisecond as something already placed - confirmed
    # 2026-11-14 (user's own concern) that two truly-simultaneous events
    # from different sources (a cycled corridor/note event and an
    # injected lyrics show/hide) risk freezing the render thread, unlike
    # the zero-GAP stacking note_pair()/mirror_note_unit already rely on
    # intentionally (e.g. 0x80 immediately followed by 0x81) - those are
    # the SAME source, in a specific known-safe order; this is about two
    # INDEPENDENT sources colliding, which is a different, unvetted
    # situation. Nudging forward by 1ms at a time is enough in practice
    # (collisions are rare single points, not dense clusters) and keeps
    # the overall timeline close enough to unaffected downstream.
    for abs_ms, e in absolute_events:
        while abs_ms in fire_times:
            abs_ms += 1
        fire_times.add(abs_ms)
        merged.append((abs_ms, 1, e))
    merged.sort(key=lambda x: (x[0], x[1]))
    out = []
    for idx, (fire_t, _, e) in enumerate(merged):
        if idx + 1 < len(merged):
            out.append(e._replace(duration_ms=merged[idx + 1][0] - fire_t))
        else:
            out.append(e)  # nothing follows in this batch - keep its own duration
    return out


def remove_events_preserving_duration(events, opcodes):
    """Drops every event whose d2 is in opcodes, folding its own
    duration_ms into the PREVIOUS kept event instead of discarding it
    (onto the next kept event if there is no previous one yet) - a plain
    list-comprehension filter silently loses that time, which is exactly
    what song.py's lyrics path did stripping leftover LINE_START/
    LINE_END markers before merging in new ones: confirmed 2026-11-14 on
    a real build, the dropped markers' own total duration (4059ms) was
    EXACTLY the gap between Song.total_ms()'s reported length and the
    actual built track's real length - the same class of drift bug as
    mirror_note_unit's, just from a different cause."""
    out = []
    pending = 0
    for e in events:
        if e.d2 in opcodes:
            pending += e.duration_ms
            continue
        if pending:
            if out:
                out[-1] = out[-1]._replace(duration_ms=out[-1].duration_ms + pending)
            else:
                # nothing kept yet (removed events led the list) - fold
                # into the upcoming event instead of the previous one.
                e = e._replace(duration_ms=e.duration_ms + pending)
            pending = 0
        out.append(e)
    if pending:
        if out:
            out[-1] = out[-1]._replace(duration_ms=out[-1].duration_ms + pending)
        else:
            out.append(Event(pending, 1, REST_OPCODE, b""))
    return out


def serialize_events(events):
    out = bytearray()
    for e in events:
        out += encode_varint(e.duration_ms)
        out.append(e.d1)
        out.append(e.d2)
        out += e.payload
    return bytes(out)


def corridor_lane_byte(midi_pitch, pitch_range=_CORRIDOR_PITCH_RANGE):
    lo, hi = pitch_range
    frac = (midi_pitch - lo) / (hi - lo)
    frac = max(0.0, min(1.0, frac))
    idx = round(frac * (len(_CORRIDOR_LANES) - 1))
    return _CORRIDOR_LANES[idx]


def corridor_event(midi_pitch, opcode=CORRIDOR_OPCODE):
    return Event(0, 2, opcode, bytes([corridor_lane_byte(midi_pitch)]))


def note_pair(duration_ms, midi_pitch):
    """The only supported way to author a melody/vocal guide note.

    Emits MELODY_ADVANCE_OPCODE (0x81) and VOCAL_ADVANCE_OPCODE (0x89)
    right after their respective note events — statistically confirmed
    2026-11-07 (decoding real tracks) to follow almost every 0x80/0x88 in
    real content, something this function never emitted before. Given at
    zero duration (unlike real tracks' own small 6-113ms values for 0x81,
    not yet understood) specifically so total_ms()'s sum-of-note-
    durations math stays exactly correct — these are pure markers here,
    not meant to add real time.

    Also wraps the note in two CORRIDOR_OPCODE (0x0D) events, mirroring
    real tracks' structure of surrounding nearly every note with pitch-
    corridor data (see corridor_lane_byte()'s docstring note above on why
    the lane byte is only an approximation, not the confirmed formula).
    Hardware symptom this is meant to fix: the on-screen note/pitch-
    corridor display rendered only one long undifferentiated note for
    the whole track - real tracks never go this long without 0x0C/0x0D,
    which our own tracks never emitted at all before this."""
    pitch = struct.pack("<H", midi_pitch)
    return [
        corridor_event(midi_pitch),
        Event(duration_ms, 3, MELODY_OPCODE, pitch),
        corridor_event(midi_pitch),
        Event(0, 1, MELODY_ADVANCE_OPCODE, b""),
        Event(0, 3, VOCAL_OPCODE, pitch),
        Event(0, 1, VOCAL_ADVANCE_OPCODE, b""),
    ]


def rest_event(duration_ms):
    """A silent gap of duration_ms with no note. Reuses the 0xE3 opcode
    seen between notes in real tracks' bodies, rather than inventing one."""
    return Event(duration_ms, 1, REST_OPCODE, b"")


def note_or_rest(duration_ms, midi_pitch):
    """midi_pitch=None -> a rest; otherwise a note_pair(). The common
    entry point for all three note-input modes (manual/midi/acapella)."""
    if midi_pitch is None:
        return [rest_event(duration_ms)]
    return note_pair(duration_ms, midi_pitch)


def split_into_note_units(events):
    """Group a real track's body events (post-preamble) into note-centered
    units: each unit is any run of non-note scaffolding (corridor/rest/
    sweep/line-marker events) immediately followed by one note's own
    MELODY_OPCODE block (0x80, optionally 0x81, optionally 0x88/0x89).
    Trailing events after the last note (if any) are appended to the
    last unit rather than dropped.

    Built for mirror_note_units() below: cycling through whole units (not
    individual events, see Song.mirror_real's docstring for why plain
    per-event cycling scrambles the melody) lets a custom song reuse a
    real template's own corridor/sweep placement relative to each note
    while still substituting the caller's own pitch/duration for the
    note itself."""
    units = []
    current = []
    leading = []
    for e in events:
        if e.d2 == MELODY_OPCODE:
            if current:
                units.append(current)
            # `leading` holds any scaffolding before the very first note
            # (real tracks often have some) - fold it into the first real
            # unit rather than leaving a note-less unit[0], which would
            # silently eat the caller's first (duration, pitch) on nothing
            # and shift every later note down by one.
            current = leading + [e]
            leading = []
        else:
            if current:
                current.append(e)
            else:
                leading.append(e)
    if current:
        current += leading
        units.append(current)
    return units


def mirror_note_unit(unit, duration_ms, midi_pitch):
    """Reuse a real note unit's own scaffolding (corridor/rest/sweep
    events verbatim), substituting midi_pitch onto the unit's own
    MELODY_OPCODE (and its paired VOCAL_OPCODE, if present).

    PROPORTIONALLY SCALES every event's duration (the note's own slot
    included) so the whole unit's output sums to EXACTLY duration_ms -
    confirmed 2026-11-14 this is required, not just a nicety: the
    earlier version only overrode the note event's own duration and left
    every surrounding scaffolding event at its template-original value,
    so each note actually cost duration_ms PLUS however much scaffolding
    time that unit happened to carry - measured on a real 668-note song,
    this inflated the real encoded track length by over 2.5x (208s of
    intended note timing became 548s of actual track time). Since the
    backing audio is only ever trimmed to the song's OWN real length,
    this is exactly the kind of bug that produces a slow, continuous
    drift against the real audio rather than a one-time offset - easy to
    misread as something tied only to rests/pauses (where it also still
    needs fixing - see mirror_rest) when it was actually present on
    every single note. Cumulative rounding (tracking the running total
    before and after rounding, not rounding each event independently)
    keeps per-event rounding error from accumulating into further drift
    across a long track."""
    pitch = struct.pack("<H", midi_pitch)
    original_total = sum(e.duration_ms for e in unit)
    scale = (duration_ms / original_total) if original_total else 0.0
    out = []
    melody_done = vocal_done = False
    acc_original = 0
    acc_scaled = 0
    for e in unit:
        acc_original += e.duration_ms
        target_cum = round(acc_original * scale)
        this_duration = target_cum - acc_scaled
        acc_scaled = target_cum
        payload = e.payload
        if e.d2 == MELODY_OPCODE and not melody_done:
            payload = pitch
            melody_done = True
        elif e.d2 == VOCAL_OPCODE and not vocal_done:
            payload = pitch
            vocal_done = True
        out.append(e._replace(duration_ms=this_duration, payload=payload))
    return out


def mirror_rest(unit_cycle, duration_ms):
    """Fills a rest of duration_ms by cycling through real note units
    with their own note/vocal events stripped out, keeping whatever
    corridor/sweep/rest scaffolding is left - confirmed 2026-11-14 that
    a bare single REST_OPCODE event (the previous behavior) left a long
    pause with ZERO 0x0C/0x0D activity, which real tracks never do
    (those opcodes fire continuously throughout a real track's body,
    not just clustered around notes) - user-reported symptoms were the
    on-screen corridor visually disappearing during a long pause, and
    everything after the pause drifting out of sync, consistent with
    the engine's own internal timing/advance logic being tied to
    processing these events rather than pure elapsed duration. Pads/
    trims the last cycled unit's own content so the total exactly
    matches duration_ms."""
    strip_opcodes = (MELODY_OPCODE, MELODY_ADVANCE_OPCODE, VOCAL_OPCODE, VOCAL_ADVANCE_OPCODE)
    out = []
    remaining = duration_ms
    empty_streak = 0
    while remaining > 0:
        unit = next(unit_cycle)
        stripped = [e for e in unit if e.d2 not in strip_opcodes]
        unit_total = sum(e.duration_ms for e in stripped)
        if unit_total == 0:
            empty_streak += 1
            if empty_streak > 64:  # every unit is note-only scaffolding-free - give up gracefully
                out.append(rest_event(remaining))
                remaining = 0
            continue
        empty_streak = 0
        if unit_total <= remaining:
            out += stripped
            remaining -= unit_total
        else:
            acc = 0
            for e in stripped:
                if acc + e.duration_ms >= remaining:
                    out.append(e._replace(duration_ms=remaining - acc))
                    acc = remaining
                    break
                out.append(e)
                acc += e.duration_ms
            remaining = 0
    return out


def extract_preamble(real_events, event3_ms=None, duration_overrides=None):
    """Split a real track's events into (preamble, rest), optionally
    overriding one or more of the preamble's 8 event durations.

    event3_ms is kept as its own parameter for backward compatibility
    (every existing caller already uses it) but is really just shorthand
    for duration_overrides={3: event3_ms} — event 3 is NOT special
    structurally, it was just the first one confirmed safe to change.
    Measuring all 8 events' durations across the complete 100-disc-song
    sample (2026-11-07) found every one of them varies freely per real
    song (not just event 3 — see project_wk_track_timing_sync.md's
    correction), and the opcodes themselves (what actually matters
    structurally) stay fixed regardless — so overriding any/all of the
    other 7 the same way should be just as safe, letting a custom song's
    intro be shortened well below any one real template's own preamble
    length. duration_overrides: optional {event_index: duration_ms},
    applied on top of (and overriding) event3_ms if both are given."""
    preamble = list(real_events[:PREAMBLE_LEN])
    overrides = {}
    if event3_ms is not None:
        overrides[3] = event3_ms
    if duration_overrides:
        overrides.update(duration_overrides)
    for idx, dur in overrides.items():
        preamble[idx] = preamble[idx]._replace(duration_ms=dur)
    return preamble, list(real_events[PREAMBLE_LEN:])


def preamble_duration_ms(preamble):
    return sum(e.duration_ms for e in preamble)


def scale_preamble_overrides(real_events, target_total_ms, event3_ms=None):
    """Builds a preamble_overrides dict (see extract_preamble) that
    proportionally scales ALL 8 preamble events (event3_ms applied
    first, same as extract_preamble itself would) so their total comes
    to exactly target_total_ms, instead of leaving 7 of the 8 at
    whatever arbitrary length the TEMPLATE song happened to use.

    Confirmed 2026-11-14 this is the actual fix needed, not just
    changing event3_ms alone: on a real song, event3_ms alone is a
    small fraction of the preamble's own total (1000 of 11376ms) - the
    other 7 events dominate the sum, so leaving them untouched barely
    moves the total even after an event3_ms change. Proportional
    scaling (not e.g. just zeroing the other 7) keeps the preamble's
    own real internal rhythm/structure, only compressed or stretched to
    fit - cumulative rounding (tracking running totals before/after
    rounding, not rounding each event independently) keeps per-event
    rounding error from drifting the final sum away from
    target_total_ms, same technique as mirror_note_unit's own scaling."""
    preamble, _rest = extract_preamble(real_events, event3_ms)
    original_total = preamble_duration_ms(preamble)
    if original_total == 0:
        return {i: 0 for i in range(len(preamble))}
    scale = target_total_ms / original_total
    overrides = {}
    acc_original = 0
    acc_scaled = 0
    for i, e in enumerate(preamble):
        acc_original += e.duration_ms
        target_cum = round(acc_original * scale)
        overrides[i] = target_cum - acc_scaled
        acc_scaled = target_cum
    return overrides


# --- JOY-U1 container ---

JoySections = namedtuple(
    "JoySections", "info palette tv_records tv_track gp_records gp_track tail"
)


def parse_joy(joy_plain):
    assert joy_plain[:6] == b"JOY-U1"
    offsets = struct.unpack(">6I", joy_plain[6:30])
    bounds = [30] + list(offsets) + [len(joy_plain)]
    parts = [joy_plain[bounds[i]:bounds[i + 1]] for i in range(7)]
    return JoySections(*parts)


def build_joy(sections: JoySections):
    parts = list(sections)
    offs = []
    pos = 30
    for p in parts[:-1]:
        pos += len(p)
        offs.append(pos)
    body = b"".join(parts)
    return b"JOY-U1" + struct.pack(">6I", *offs) + body
