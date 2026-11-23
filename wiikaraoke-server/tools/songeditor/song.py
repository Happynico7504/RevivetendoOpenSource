"""
High-level Song builder, tying together gamedata/container/sszl/track/audio.

This is the scaffolding piece. It reuses a template real song's own palette,
lyric records, font and tail verbatim — no lyric-sweep timing or custom text
yet (see track.py's module docstring) — and only replaces the TV/GamePad
track with freshly-authored melody notes, and the audio with the caller's
own track. A background PNG can come from any existing UJK0 file, plain or
real, since it doesn't need to match the template's track content at all.
"""
from . import audio as audio_mod
from . import container
from . import gamedata
from . import lyrics as lyrics_mod
from . import sszl
from . import titlecard
from . import track


class Song:
    def __init__(self, notes, event3_ms=1000, preamble_overrides=None, mirror_real=None,
                 lyrics_lines=None):
        """notes: list of (duration_ms, midi_pitch) for the melody — the
        paired vocal-guide events are generated automatically. midi_pitch
        may be None for a rest. Use songeditor.notes to build this list
        from manual entries, a MIDI file, or a cappella audio.

        preamble_overrides: optional {event_index: duration_ms} for any
        of the preamble's 8 events (0-7), not just event 3 — see
        track.extract_preamble's docstring. Pass this (with a short
        duration for every index, e.g. all 8 at ~150-200ms) to shrink a
        custom song's intro well below any one real template's own
        preamble length.

        mirror_real: ACCEPTED BUT IGNORED, kept only so existing callers
        (cli.py, any already-saved spec JSON) that still pass it don't
        break - the behavior it used to gate is now unconditional. It
        was an opt-in flag that defaulted to False everywhere (here, in
        cli.py, in the Go spec struct), and the actual editor UI never
        set it at all - confirmed 2026-11-14 this meant every song built
        through the real editor (not a one-off hand-edited spec) still
        hit the OLD, broken note_pair()-only synthesis, the exact "first
        note shows, nothing else" bug this was meant to fix in the first
        place. There is no remaining reason to prefer the old path over
        this one, so it's no longer a choice.

        Reuses the TEMPLATE's own real body events (everything after the
        8-event preamble) grouped into note-centered units (track.
        split_into_note_units) - each unit being one note's own real
        corridor/sweep/rest scaffolding plus its MELODY_OPCODE/
        VOCAL_OPCODE block - instead of synthesizing fresh note_pair()
        events, which never emit the corridor/sweep opcodes (see
        track.py's module docstring). Cycling through whole UNITS (not
        individual events - an earlier attempt at that confirmed
        scrolling works but scrambled the melody, since most cycle
        positions in a real track are non-note events) and substituting
        the caller's own duration_ms/midi_pitch only onto each unit's
        own note (track.mirror_note_unit) keeps the real corridor/sweep
        placement and proportions relative to notes while actually
        playing the caller's intended melody.

        lyrics_lines: optional list of (text, start_ms, end_ms) - absolute
        track time, same timeline as preamble+notes (t=0 is the very
        start of the track, before the preamble - the same zero point
        self.total_ms() measures from). When given, a NEW font (section
        3) and tv_records/gp_records are generated (lyrics.build_font/
        build_records) instead of borrowing the template's, and exactly
        one LINE_START/LINE_END pair per line is merged into the track
        at the given times (track.interleave_absolute) - see lyrics.py's
        module docstring for why this fixes a real, previously-unresolved
        bug (mismatched show/hide count, overlapping adjacent lines) in
        the reference script this was ported from."""
        self.notes = notes
        self.event3_ms = event3_ms
        self.preamble_overrides = preamble_overrides
        self.lyrics_lines = lyrics_lines

    def _extract_preamble(self, real_track_events):
        return track.extract_preamble(
            real_track_events, self.event3_ms, self.preamble_overrides
        )

    def _events(self, real_track_events):
        """The full preamble+body event list, NOT yet merged with lyrics
        or serialized - shared by total_ms() callers and build() alike
        so there's exactly one place that builds the note/rest body."""
        preamble, rest = self._extract_preamble(real_track_events)
        events = list(preamble)
        import itertools
        units = track.split_into_note_units(rest)
        cycle = itertools.cycle(units)
        for duration_ms, pitch in self.notes:
            if pitch is None:
                # mirror_rest consumes as many units from the cycle as it
                # needs on its own (a long rest needs several) - unlike
                # the note case below, which always needs exactly one.
                events += track.mirror_rest(cycle, duration_ms)
            else:
                unit = next(cycle)
                events += track.mirror_note_unit(unit, duration_ms, pitch)
        return preamble, events

    def total_ms(self, real_track_events):
        """The REAL encoded track length, not an approximation - confirmed
        2026-11-14 that `preamble + sum(self.notes)` (the old formula)
        badly underestimates this: mirror_note_unit keeps a unit's own
        surrounding scaffolding at ITS original duration and adds the
        caller's note duration ON TOP of it (by design, to preserve real
        corridor/sweep timing), so the actual per-note cost is always
        MORE than just the note's own stated duration. Measured on a
        real 668-note song: the old formula said 208176ms, the actual
        encoded track was 547887ms - over 2.5x longer. Since that total
        drives how much backing audio gets kept (cli.py's trim step),
        underestimating it left the track running past where the trimmed
        audio ends, a real (if non-obvious) cause behind reports of
        things drifting out of sync, worse later in a song - calling
        _events() directly instead of re-deriving an approximation makes
        this exactly right by construction, not just more accurate."""
        _preamble, events = self._events(real_track_events)
        return sum(e.duration_ms for e in events)

    def build(self, rpx_path, template_real_ujk_path, raw_audio_adts, out_path,
              png_bytes=None, font_bytes=None, title=None, artist=None,
              writer=None, composer=None):
        """
        rpx_path: path to the game dump's code/app.rpx.
        template_real_ujk_path: a real, on-disc .ujk to borrow the
            palette/records/font/tail (and PNG/font, unless overridden) from.
        raw_audio_adts: this song's own audio, already encoded to plain
            AAC-LC ADTS (audio.encode_to_adts) and trimmed so its length
            matches self.total_ms(...) (audio.trim_to_frame_boundary) —
            NOT yet wrapped in the TPSA header/packets.
        png_bytes: background image bytes; borrowed from the template if omitted.
        font_bytes: SSZL-compressed font section bytes (as stored in a UJK0
            file's slot 3); borrowed from the template if omitted.
        title/artist/writer/composer: if png_bytes is omitted AND title is
            given, a title card is generated (titlecard.build()) instead of
            borrowing the template's — confirmed 2026-11-07 that the
            in-game title/credits text is baked into the PNG as a graphic,
            not read from any database, so every custom song needs its own
            or it keeps showing whatever real song the template came from.
        """
        xk = gamedata.xor_table(rpx_path)
        key = gamedata.blowfish_key(rpx_path)

        real = container.read_real(template_real_ujk_path, key, xk)
        real_table = container.read_table(real, xk)
        joy_plain = sszl.decompress(container.read_section(real, real_table, 2))
        joy = track.parse_joy(joy_plain)
        real_track_events = track.parse_events(joy.tv_track)

        _preamble, events = self._events(real_track_events)

        new_records = None
        if self.lyrics_lines:
            # 10, not just 1 - the custom catalog bring-up earlier this
            # session hit the exact same class of bug ("a catalog with
            # too few total rows hangs the main menu - empirically needed
            # >=~10 songs", see this file's 2026-11-14 catalog entry
            # above) and real songs' own lyric line counts are always
            # comfortably above 10 (28-163 seen across 20 real songs) -
            # a single trailing row may just move the same minimum-count
            # requirement rather than satisfy it.
            texts = lyrics_mod.pad_lines([t for t, _, _ in self.lyrics_lines], pad_to=10)
            font_bytes_raw, index_map, large_glyphs = lyrics_mod.build_font(texts)
            if font_bytes is None:
                font_bytes = sszl.compress(font_bytes_raw)
            new_records = lyrics_mod.build_records(texts, index_map, large_glyphs, joy.tv_records[:30])
            # mirror_real's note-unit cycling naturally reuses whatever
            # real LINE_START/LINE_END markers happen to fall inside the
            # template's own cycled body events - fine when the template's
            # OWN tv_records is also left untouched (its line count still
            # matches), but here we're replacing tv_records with a
            # different line count entirely, so those leftover markers
            # must be dropped first - otherwise the track emits more
            # LINE_START events than the new records table has lines,
            # the exact count-mismatch that caused a hard hang earlier
            # this session (see the records-table docstring note below).
            events = track.remove_events_preserving_duration(
                events, (track.LINE_START_OPCODE, track.LINE_END_OPCODE))
            show_hide = lyrics_mod.show_hide_events(
                [(s, e) for _, s, e in self.lyrics_lines],
                note_list=self.notes, preamble_ms=track.preamble_duration_ms(_preamble))
            events = track.interleave_absolute(events, show_hide)

        new_track = track.serialize_events(events)
        # tv_records/gp_records: confirmed 2026-11-14 this is a per-lyric-
        # line table (30-byte global header, then one variable-length
        # record per line - see lyrics.py's module docstring for the full
        # field layout). Borrowed from the template untouched UNLESS
        # lyrics_lines regenerates it - the template's own tv_records only
        # makes sense paired with however many LINE_START (0xC0) events
        # the TRACK emits; mirror_real's cycling naturally reuses the
        # template's own real 0xC0/0xC1 markers in proportion, which is
        # why the template's untouched records still work in that case.
        # Resizing this table while mirror_real's cycling still emits the
        # ORIGINAL template's own 0xC0 count caused a hard hang on real
        # hardware - keep both sides in sync (lyrics_lines' show_hide_
        # events() generates exactly one LINE_START/LINE_END pair per new
        # line) if you ever change one without the other.
        if new_records is not None:
            new_joy = track.build_joy(joy._replace(
                tv_records=new_records, tv_track=new_track,
                gp_records=new_records, gp_track=new_track,
            ))
        else:
            new_joy = track.build_joy(joy._replace(tv_track=new_track, gp_track=new_track))
        joy_compressed = sszl.compress(new_joy)
        assert sszl.decompress(joy_compressed) == new_joy

        if png_bytes is None and title is not None:
            png_bytes = titlecard.build(title, artist, writer, composer)
        if png_bytes is None:
            png_bytes = container.read_section(real, real_table, 1)
        if font_bytes is None:
            font_bytes = container.read_section(real, real_table, 3)

        tpsa = audio_mod.build_tpsa(raw_audio_adts)

        out = container.build({0: tpsa, 1: png_bytes, 2: joy_compressed, 3: font_bytes}, xk)
        open(out_path, "wb").write(out)
        return out
