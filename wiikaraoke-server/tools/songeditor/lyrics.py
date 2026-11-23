"""
Generates custom lyric lines: a font/glyph block (section 3) and matching
tv_records/gp_records, instead of borrowing a template's.

Ported 2026-11-14 from a 2026-11-02/03 scratch script (`mk_orig.py`) that
already proved the font format correct via a real hardware test ("the
font never was the problem") - that investigation's actual unresolved bug
was that its line-show/hide (0xC0/0xC1) event count never matched its own
line count (4 melody entries driving show/hide timing vs 5 lyric lines),
so the extra line could never show and adjacent lines' show/hide events
overlapped instead of strictly alternating. This module fixes both: the
caller supplies exactly one (start_ms, end_ms) pair per line, and
show_hide_events() builds exactly one 0xC0/0xC1 pair per line to match.

Record format (confirmed 2026-11-14 against a real song, see
project_wii_karaoke.md): a per-song 30-byte header (must be copied
verbatim from whatever template's tv_records the caller is building on -
see song.py) followed by N lines of:
  u16 selfLen (includes itself), s16 s16 s16 (layout/timing, individually
  unidentified - kept at 0/y/width like the proven-safe reference script),
  4 bytes (flags, constant 0x00020100 in every real line checked),
  s16 s16 (x/y pixel position), u16 n1 (char count),
  n1 x {u8 styleFlag, u16 glyphTableIndex}, u16 n2 (ruby count, always 0
  here - ruby/furigana groups are real but not needed for non-Japanese
  custom text).

Font format: two blocks (large single-screen-sized glyphs, ident
0x00020001; small ones for a secondary lane, ident 0x00080001), both
built from the SAME TTF, one glyph per unique character across all lines.
Confirmed structurally against the full 100-real-song corpus on
2026-11-03 before this ported script's hardware test, so the format
itself was never in doubt - only the show/hide timing was.
"""
import bisect
import struct

import numpy as np
from PIL import Image, ImageDraw, ImageFont

from . import track

# Real songs clear a line BEFORE a long rest, not when it coincides with
# the rest's own start, and not at the line's own sweep-completion time
# (see show_hide_events's 2026-11-14 note below) - a census of 8 long
# rests (>=800ms) across 5 real discs found LINE_END 100-800ms before
# the rest in 6/8 cases (the other 2 simply had no line showing to
# clear). REST_CLEAR_THRESHOLD_MS/REST_CLEAR_LEAD_MS match that.
REST_CLEAR_THRESHOLD_MS = 500
REST_CLEAR_LEAD_MS = 300

DEFAULT_TTF = "/usr/share/fonts/truetype/dejavu/DejaVuSans-Bold.ttf"
LARGE_IDENT = 0x00020001
SMALL_IDENT = 0x00080001
LARGE_SIZE = (44, 55)  # (pt size, line height) - matches the reference script
SMALL_SIZE = (26, 32)
HALO = {LARGE_IDENT: 3, SMALL_IDENT: 2}


def _build_glyph(ch, size, lh, halo, ttf_path):
    font = ImageFont.truetype(ttf_path, size)
    adv = max(int(round(font.getlength(ch))), 8)
    if ch == " ":
        return dict(code=0x20, adv=adv, lh=lh, w=2, h=2, row=1, xo=0, yo=0, px=b"\xff\xff")
    w_canvas, h_canvas = adv + 2 * halo + 8, lh + 2 * halo + 8
    fill_img = Image.new("L", (w_canvas, h_canvas), 0)
    stroke_img = Image.new("L", (w_canvas, h_canvas), 0)
    ImageDraw.Draw(fill_img).text((halo + 4, halo + 4), ch, font=font, fill=255)
    ImageDraw.Draw(stroke_img).text((halo + 4, halo + 4), ch, font=font, fill=255,
                                     stroke_width=halo, stroke_fill=255)
    x0, y0, x1, y1 = stroke_img.getbbox()
    x1 = x0 + ((x1 - x0 + 1) & ~1)  # even width - pixels are packed 2-per-byte below
    fill_img = fill_img.crop((x0, y0, x1, y1))
    stroke_img = stroke_img.crop((x0, y0, x1, y1))
    w, h = fill_img.size
    fill_arr = np.array(fill_img)
    stroke_arr = np.array(stroke_img)
    # 4bpp nibbles: 15=transparent, 14=halo edge, 0=full ink, linear ramp between
    nibbles = np.full((h, w), 15, dtype="u1")
    nibbles[stroke_arr > 64] = 14
    inked = fill_arr > 0
    nibbles[inked] = np.clip(14 - (fill_arr[inked].astype(int) * 15) // 255, 0, 14)
    row_bytes = w // 2
    px = bytes(((int(nibbles[y, x]) << 4) | int(nibbles[y, x + 1]))
               for y in range(h) for x in range(0, w, 2))
    return dict(code=ord(ch), adv=adv, lh=lh, w=w, h=h, row=row_bytes,
                xo=x0 - (halo + 4), yo=y0 - (halo + 4), px=px)


def _build_block(ident, glyphs):
    table_off = 0x14
    table_size = 4 * len(glyphs)
    data_off = table_off + table_size
    records = []
    offsets = []
    pos = data_off
    for g in glyphs:
        rec = (bytes.fromhex("ffff000000000000")
               + struct.pack(">HBBBBH", g["code"], g["adv"], g["lh"], g["w"], g["h"], g["row"])
               + struct.pack(">bBbB", g["xo"], 4, g["yo"], 4)
               + b"\0\0"
               + struct.pack("<H", len(g["px"])) + g["px"])
        offsets.append(pos)
        records.append(rec)
        pos += len(rec)
    data = b"".join(records)
    return (struct.pack(">IIIII", ident, table_off, table_size, data_off, len(data))
            + struct.pack(">%dI" % len(glyphs), *offsets) + data)


def pad_lines(lines, pad_to=True):
    """Apply the same trailing-sacrificial-row padding build_records()
    would, so build_font() sees the padding character(s) too - call this
    once and pass its result to BOTH build_font() and build_records()
    (with pad_to left at its default there, since padding already
    happened here) rather than letting build_records() pad on its own.

    Checked across 20 real songs (2026-11-14) for how Nintendo/JOYSOUND
    represent an invisible placeholder line - found no example at all
    (every real line has n1>=1, no unusual flag values); real songs
    likely just have so many lines (28-163 seen) that a normal
    playthrough never reaches the true last one, so the engine's
    last-line bug (if that's what this is) never gets triggered in
    practice. With no real convention to copy, the empty string (n1=0,
    zero glyph entries - nothing at all to render, not even a space
    glyph) is the most clearly "invisible" representation available and
    needs no character to already exist in index_map."""
    if pad_to is True:
        return list(lines) + [""]
    if pad_to and len(lines) < pad_to:
        return list(lines) + [""] * (pad_to - len(lines))
    return list(lines)


def build_font(lines, ttf_path=DEFAULT_TTF):
    """lines: list of strings. Returns (font_bytes, index_map) where
    index_map maps each unique character to its glyph-table index (shared
    across both the large and small blocks - both blocks list the same
    characters in the same order, only their pixel size differs)."""
    chars = sorted(set("".join(lines)))
    index_map = {c: i for i, c in enumerate(chars)}
    large = [_build_glyph(c, LARGE_SIZE[0], LARGE_SIZE[1], HALO[LARGE_IDENT], ttf_path) for c in chars]
    small = [_build_glyph(c, SMALL_SIZE[0], SMALL_SIZE[1], HALO[SMALL_IDENT], ttf_path) for c in chars]
    block1 = _build_block(LARGE_IDENT, large)
    block2 = _build_block(SMALL_IDENT, small)
    font_bytes = struct.pack(">IIII", 0x10, 0x10 + len(block1), len(block1), len(block2)) + block1 + block2
    return font_bytes, index_map, large


# Screen rows to cycle lines through, well-spaced so up to 4 lines can be
# simultaneously visible (confirmed 2026-11-14: real songs do show 3-4
# lines on screen at once before clearing, each at a different row - our
# first attempt put every line at y=its own array index, i.e. y=0,1,2...,
# stacking them almost on top of each other instead of using distinct
# rows like this).
#
# Narrowed twice now. First pass (100,250,400,480) was picked from a
# 15-disc/1197-line census of real y values (span 14-590, density
# falling off above ~500) - that range sits well inside what real discs
# themselves use, but a hardware test still showed some of OUR lines
# landing off-screen at those same values. A wider, 100-file/7588-line
# re-census confirmed the real-disc distribution itself isn't the
# limiting factor (p10=30, p90=460 - 100-480 is comfortably inside it) -
# so whatever's clipping is specific to how OUR synthetic glyphs/records
# render relative to a given y (build_font's LARGE_SIZE vs whatever a
# real disc's own font actually is), not to y value choice alone. Since
# that can't be resolved from census data, narrowed further and shifted
# toward the vertical center, away from both edges, rather than trying
# to compute an exact safe bound from clearly-insufficient information.
SCREEN_ROWS = [160, 260, 340, 420]


def build_records(lines, index_map, large_glyphs, header30, screen_width=1280, rows=SCREEN_ROWS):
    """header30: the template's own first 30 bytes of tv_records/gp_records
    - a fixed per-song header that must be preserved verbatim (confirmed
    2026-11-14: not understood field-by-field, but swapping it causes no
    issue when copied as-is; never regenerate it from scratch).

    rows: screen Y positions, one per simultaneously-visible line slot,
    cycled through in order (lines[0] uses rows[0], lines[1] uses
    rows[1], lines[len(rows)] wraps back to rows[0], etc.) - matches real
    songs' own convention of giving each of several on-screen-at-once
    lines a distinct row rather than overlapping them.

    lines must already include any trailing sacrificial padding row (see
    pad_lines()) - confirmed 2026-11-14 the crash always hits whichever
    line is PHYSICALLY LAST in the array, regardless of total count
    (padding to 4 entries just moved the crash from line 2 to line 4) -
    the same bug class already seen in the catalog database work earlier
    this session, where a single trailing padding row was the fix."""
    out = bytearray(header30)
    for i, line in enumerate(lines):
        y = rows[i % len(rows)]
        advance = sum(large_glyphs[index_map[c]]["adv"] for c in line)
        width = int(advance * 1.245)  # matches the reference script's own fudge factor
        x = max(0, (screen_width - width) // 2)
        entries = b"".join(bytes([0]) + struct.pack(">H", index_map[c]) for c in line)
        body = (struct.pack(">hhh", 0, y, width) + bytes([0, 2, 1, 0])
                + struct.pack(">hh", x, y) + struct.pack(">H", len(line)) + entries
                + struct.pack(">H", 0))  # n2 = 0, no ruby groups
        selflen = 2 + len(body)
        out += struct.pack(">H", selflen) + body
    return bytes(out)


def show_hide_events(line_times, note_list=None, preamble_ms=0, emit_hide=False):
    """line_times: list of (start_ms, end_ms) absolute times, one per
    line, in track-timeline order. Returns a list of (abs_ms, Event)
    tuples for track.interleave_absolute().

    2026-11-14: a hardware test with emit_hide=True (one LINE_END per
    line, timed at each line's own end_ms) rendered the line's text
    correctly but then crashed exactly when its sweep/fill animation
    reached completion - user's own explanation: the game can have
    multiple lines active/visible at once without ever being told to
    clear them, as long as no two lines ever share the engine's
    internal "current line" tracking at the same time; explicitly
    firing LINE_END right as a line's own sweep completes naturally is
    likely DOUBLE-clearing against the engine's own completion-driven
    logic, not providing a needed signal. emit_hide now defaults to
    False and exists only for backward compatibility - don't pass True.

    note_list/preamble_ms (2026-11-14, same day, following up on a
    hardware test that hung during a long lyrics-free stretch): real
    discs DO clear an active line explicitly, but tied to a long REST
    in the NOTE track, not to the line's own end_ms - a census of real
    rests >=800ms found LINE_END firing 100-800ms before the rest, not
    at it and not at the line's own natural completion. When note_list
    is given, this scans it for rests >=REST_CLEAR_THRESHOLD_MS and, for
    each one that falls within some line's SHOW-only display window
    (its own start_ms up to the next line's start_ms), emits a LINE_END
    REST_CLEAR_LEAD_MS before the rest begins - clamped to never land
    before that line's own start_ms, so a rest immediately after a line
    starts can't clear it instantly. At most one LINE_END per line - a
    long display window can contain several qualifying rests (seen in
    practice), and real data never showed more than one clear between
    two LINE_STARTs, so only the first is emitted."""
    events = []
    for start_ms, end_ms in line_times:
        if emit_hide and end_ms <= start_ms:
            raise ValueError("line end_ms must be after its own start_ms")
        events.append((start_ms, track.Event(0, 1, track.LINE_START_OPCODE, b"")))
        if emit_hide:
            events.append((end_ms, track.Event(0, 1, track.LINE_END_OPCODE, b"")))
    if emit_hide:
        for i in range(len(line_times) - 1):
            if line_times[i][1] > line_times[i + 1][0]:
                raise ValueError("line %d's end_ms overlaps line %d's start_ms" % (i, i + 1))

    if note_list is not None and line_times:
        starts = [s for s, _ in line_times]
        cleared = set()  # at most one LINE_END per line - see docstring
        cum = preamble_ms
        for duration_ms, pitch in note_list:
            if pitch is None and duration_ms >= REST_CLEAR_THRESHOLD_MS:
                i = bisect.bisect_right(starts, cum) - 1
                if i >= 0 and i not in cleared:
                    window_end = line_times[i + 1][0] if i + 1 < len(line_times) else None
                    if window_end is None or cum < window_end:
                        clear_at = max(line_times[i][0] + 1, cum - REST_CLEAR_LEAD_MS)
                        if window_end is None or clear_at < window_end:
                            events.append((clear_at, track.Event(0, 1, track.LINE_END_OPCODE, b"")))
                            cleared.add(i)
            cum += duration_ms
    return events
