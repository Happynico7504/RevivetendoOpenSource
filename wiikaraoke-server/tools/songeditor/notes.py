"""
Three ways to produce the (duration_ms, midi_pitch) note list that
song.Song expects (midi_pitch=None means a rest):

  from_manual(entries)     — you specify every note yourself.
  from_midi(path, ...)     — import a melody track from a .mid file.
  from_acapella(path, ...) — pitch-track an isolated vocal/melody audio
                              file and segment it into notes automatically.

from_midi and from_acapella are both best-effort reductions to a single
monophonic line — this editor (and the game's own guide-note format) has
no notion of chords or multiple simultaneous voices.
"""
import struct

import numpy as np

_NOTE_BASE = {"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}

# track.mirror_rest() is only ever confirmed safe up to what real discs
# themselves actually use - a census of real long rests across several
# discs found a max of 2297ms. A from_acapella_ml rest can run far
# longer than that (one real case: 6020ms, from the pitch detector
# missing real singing outright, not a genuine pause) - song._events()
# calls mirror_rest() once per rest NOTE, not merged across consecutive
# ones, so cap_rest_length (below) splitting one long rest into several
# shorter ones keeps every individual mirror_rest() call inside the
# range real data validates, without changing the song's audible/visual
# content (still the same total rest time either way).
MAX_REST_MS = 2000


def cap_rest_length(note_list, max_rest_ms=MAX_REST_MS):
    out = []
    for duration_ms, pitch in note_list:
        if pitch is None and duration_ms > max_rest_ms:
            remaining = duration_ms
            while remaining > 0:
                chunk = min(remaining, max_rest_ms)
                out.append((chunk, None))
                remaining -= chunk
        else:
            out.append((duration_ms, pitch))
    return out


def note_name_to_midi(name):
    """"C4" -> 60 (scientific pitch notation, MIDI note 60 = C4 = middle C)."""
    name = name.strip()
    letter = name[0].upper()
    i = 1
    accidental = 0
    while i < len(name) and name[i] in "#b":
        accidental += 1 if name[i] == "#" else -1
        i += 1
    octave = int(name[i:])
    return _NOTE_BASE[letter] + accidental + (octave + 1) * 12


# --- manual mode ---

def from_manual(entries):
    """entries: list of (duration_ms, pitch) where pitch is a MIDI number,
    a note-name string like "C#4", or None for a rest."""
    out = []
    for duration_ms, pitch in entries:
        if isinstance(pitch, str):
            pitch = note_name_to_midi(pitch)
        out.append((int(duration_ms), pitch))
    return out


# --- MIDI mode ---

def _read_varlen(data, p):
    value = 0
    while True:
        b = data[p]
        p += 1
        value = (value << 7) | (b & 0x7F)
        if not (b & 0x80):
            return value, p


def _parse_midi_track(data):
    """Yields (abs_tick, event) where event is ('on', pitch, velocity),
    ('off', pitch) or ('tempo', us_per_quarter)."""
    p = 0
    tick = 0
    running_status = None
    n = len(data)
    events = []
    while p < n:
        delta, p = _read_varlen(data, p)
        tick += delta
        status = data[p]
        if status < 0x80:
            # running status: reuse the previous status byte, this byte is data
            status = running_status
        else:
            p += 1
            running_status = status if status < 0xF0 else running_status
        kind = status & 0xF0
        if status == 0xFF:
            meta_type = data[p]
            p += 1
            length, p = _read_varlen(data, p)
            payload = data[p:p + length]
            p += length
            if meta_type == 0x51 and length == 3:
                us = (payload[0] << 16) | (payload[1] << 8) | payload[2]
                events.append((tick, ("tempo", us)))
        elif status in (0xF0, 0xF7):
            length, p = _read_varlen(data, p)
            p += length
        elif kind in (0x80, 0x90):
            pitch, vel = data[p], data[p + 1]
            p += 2
            if kind == 0x90 and vel > 0:
                events.append((tick, ("on", pitch, vel)))
            else:
                events.append((tick, ("off", pitch)))
        elif kind in (0xA0, 0xB0, 0xE0):
            p += 2
        elif kind in (0xC0, 0xD0):
            p += 1
        else:
            raise ValueError("unhandled MIDI status byte %#x at offset %d" % (status, p))
    return events


def _load_midi_tracks(path):
    data = open(path, "rb").read()
    assert data[:4] == b"MThd"
    header_len = struct.unpack(">I", data[4:8])[0]
    _fmt, ntrks, division = struct.unpack(">HHH", data[8:8 + header_len])
    if division & 0x8000:
        raise ValueError("SMPTE-based MIDI timing is not supported")
    ticks_per_quarter = division

    tracks = []
    p = 8 + header_len
    for _ in range(ntrks):
        assert data[p:p + 4] == b"MTrk"
        length = struct.unpack(">I", data[p + 4:p + 8])[0]
        tracks.append(_parse_midi_track(data[p + 8:p + 8 + length]))
        p += 8 + length
    return tracks, ticks_per_quarter


def from_midi(path, track_index=None, min_rest_ms=1):
    """Imports a single monophonic melody line from a Standard MIDI File.
    If track_index is omitted, merges every track that has at least one
    note-on (skipping tempo/meta-only tracks). A single melodic line is
    often exported split across several tracks - one per section/verse,
    not one track with everything - so picking only the track with the
    most notes (an earlier version of this function did that) silently
    drops the others; merging and letting the monophonic reduction below
    sort out any real overlap is the safer default. Pass an explicit
    track_index to use just one track instead. Overlapping notes (from
    genuine chords/harmony, within one track or across merged ones) are
    reduced monophonically: a new note-on cuts the currently-sounding
    note short."""
    tracks, ticks_per_quarter = _load_midi_tracks(path)

    if track_index is not None:
        events = sorted(tracks[track_index], key=lambda te: te[0])
    else:
        merged = []
        for t in tracks:
            if any(e[0] == "on" for _, e in t):
                merged.extend((tick, e) for tick, e in t if e[0] in ("on", "off"))
        events = sorted(merged, key=lambda te: te[0])

    # build a tempo map from ALL tracks (format-1 files put tempo on track 0)
    tempo_changes = sorted(
        (tick, e[1]) for t in tracks for tick, e in t if e[0] == "tempo"
    )
    if not tempo_changes or tempo_changes[0][0] != 0:
        tempo_changes.insert(0, (0, 500000))  # default 120 BPM

    def tick_to_ms(tick):
        ms = 0.0
        last_tick, last_tempo = tempo_changes[0]
        for change_tick, tempo in tempo_changes[1:]:
            if change_tick >= tick:
                break
            ms += (change_tick - last_tick) * last_tempo / ticks_per_quarter / 1000
            last_tick, last_tempo = change_tick, tempo
        ms += (tick - last_tick) * last_tempo / ticks_per_quarter / 1000
        return ms

    notes = []  # (start_tick, end_tick, pitch)
    current = None  # (start_tick, pitch)
    for tick, e in events:
        if e[0] == "on":
            if current is not None:
                notes.append((current[0], tick, current[1]))
            current = (tick, e[1])
        elif e[0] == "off" and current is not None and e[1] == current[1]:
            notes.append((current[0], tick, current[1]))
            current = None
    if current is not None:
        notes.append((current[0], events[-1][0] if events else current[0], current[1]))

    out = []
    prev_end_ms = 0.0
    for start_tick, end_tick, pitch in notes:
        start_ms = tick_to_ms(start_tick)
        end_ms = tick_to_ms(end_tick)
        gap_ms = round(start_ms - prev_end_ms)
        if gap_ms >= min_rest_ms:
            out.append((gap_ms, None))
        out.append((round(end_ms - start_ms), pitch))
        prev_end_ms = end_ms
    return out


# --- a cappella / automatic mode ---

def _autocorrelation_f0(frame, sr, fmin=70.0, fmax=1000.0):
    frame = frame - np.mean(frame)
    if np.max(np.abs(frame)) < 1e-6:
        return 0.0, 0.0
    corr = np.correlate(frame, frame, mode="full")[len(frame) - 1:]
    corr /= corr[0] + 1e-9
    lag_min = int(sr / fmax)
    lag_max = min(int(sr / fmin), len(corr) - 1)
    if lag_max <= lag_min:
        return 0.0, 0.0
    segment = corr[lag_min:lag_max]
    peak = int(np.argmax(segment)) + lag_min
    clarity = corr[peak]
    if peak == 0:
        return 0.0, 0.0
    return sr / peak, float(clarity)


def _hz_to_midi(hz):
    return 69 + 12 * np.log2(hz / 440.0)


def _segment_pitch_track(rms_all, midi_all, hop_ms, silence_rms_ratio, min_note_ms):
    """Shared by from_acapella (autocorrelation) and from_acapella_ml
    (torchcrepe) - everything AFTER per-frame pitch detection is
    identical regardless of which detector produced midi_all: silence
    thresholding, rounding to the nearest semitone, median-smoothing
    over 3 frames to reduce single-frame flutter, run-length segmenting,
    and merging fragments shorter than min_note_ms into a neighbor."""
    loud_rms = np.median([r for r in rms_all if r > 0]) if rms_all else 0.0
    threshold = loud_rms * silence_rms_ratio
    rounded = []
    for rms, midi in zip(rms_all, midi_all):
        if midi is None or rms < threshold:
            rounded.append(None)
        else:
            rounded.append(int(round(midi)))

    smoothed = list(rounded)
    for i in range(1, len(rounded) - 1):
        window = [v for v in rounded[i - 1:i + 2] if v is not None]
        if len(window) == 3 and len(set(window)) > 1:
            smoothed[i] = sorted(window)[1]

    segments = []  # [pitch_or_None, frame_count]
    for v in smoothed:
        if segments and segments[-1][0] == v:
            segments[-1][1] += 1
        else:
            segments.append([v, 1])

    min_frames = max(1, round(min_note_ms / hop_ms))
    i = 0
    while i < len(segments):
        if segments[i][1] < min_frames and len(segments) > 1:
            neighbor = i - 1 if i > 0 else i + 1
            segments[neighbor][1] += segments[i][1]
            del segments[i]
            if neighbor > i:
                neighbor -= 1
            i = neighbor
        else:
            i += 1

    return [(round(count * hop_ms), pitch) for pitch, count in segments]


def detect_lead_in_ms(audio_path, frame_ms=40, hop_ms=10, loud_rms_ratio=0.15, sustain_ms=200):
    """Finds where real singing actually starts in an isolated vocal/
    melody recording - an RMS-only onset detector (no pitch tracking
    needed, so this is fast even without torchcrepe), looking for the
    first point where loudness stays above loud_rms_ratio of the
    recording's own loud-section median for at least sustain_ms in a
    row (a single loud blip isn't enough - avoids false-triggering on a
    click/pop at the very start).

    Confirmed 2026-11-14 this matters: skip_ms (how much of the
    recording's start to discard before note/lyric extraction) was
    previously tied to the TEMPLATE song's own fixed preamble length
    (11376ms for 0000113), which has no relationship to any given
    recording's own actual lead-in - on a real recording whose singing
    started at ~7.4s, skipping 11.4s meant note extraction started
    already mid-phrase, well past where the lyrics' own (independently
    computed) timestamps expected the first line to begin. See
    track.scale_preamble_overrides for turning this into an actual
    preamble_overrides dict."""
    sr = 22050
    data = _decode_mono_pcm(audio_path, 0, sr)
    win = int(frame_ms / 1000 * sr)
    hop = int(hop_ms / 1000 * sr)
    rms_all = [
        float(np.sqrt(np.mean(data[i:i + win] ** 2)))
        for i in range(0, max(1, len(data) - win), hop)
    ]
    if not rms_all:
        return 0
    loud_rms = np.median([r for r in rms_all if r > 0]) or 0.0
    threshold = loud_rms * loud_rms_ratio
    sustain_frames = max(1, round(sustain_ms / hop_ms))
    run = 0
    for i, r in enumerate(rms_all):
        if r >= threshold:
            run += 1
            if run >= sustain_frames:
                return max(0, round((i - sustain_frames + 1) * hop_ms))
        else:
            run = 0
    return 0  # never found a sustained loud section - don't skip anything


def _decode_mono_pcm(audio_path, skip_ms, sr):
    import subprocess
    import tempfile
    import wave

    with tempfile.NamedTemporaryFile(suffix=".wav") as tmp:
        subprocess.run(
            ["ffmpeg", "-v", "error", "-y", "-ss", str(skip_ms / 1000), "-i", audio_path,
             "-ar", str(sr), "-ac", "1", tmp.name],
            check=True,
        )
        w = wave.open(tmp.name, "rb")
        n = w.getnframes()
        return np.frombuffer(w.readframes(n), dtype=np.int16).astype(np.float64)


def from_acapella(
    audio_path,
    skip_ms=0,
    frame_ms=40,
    hop_ms=10,
    min_note_ms=60,
    silence_rms_ratio=0.1,
    min_clarity=0.6,
):
    """Best-effort monophonic pitch-tracking of an isolated vocal/melody
    recording, decoded via ffmpeg. Heuristic: autocorrelation F0 per frame
    (see from_acapella_ml for a neural, far more accurate alternative),
    rounded to the nearest semitone, median-smoothed, segmented into notes
    wherever the rounded pitch changes, with short fragments merged into
    their neighbor. Octave errors and vibrato smoothing are known
    limitations — always expect to touch up the result in manual mode.

    skip_ms: how much of the START of the recording to discard before
    pitch-tracking — must equal the song's synthetic preamble duration
    (track.preamble_duration_ms). Song.build() always prepends that
    preamble ahead of these notes, so without this the recording's own
    leading silence/intro gets counted twice (once by the preamble, once
    by this function tracking it as more "song" from sample 0) and the
    total performance length overshoots the real backing track — the
    same "audio and notes must share one offset" principle documented in
    project_wk_audio_streaming_format.md for MIDI onset detection,
    applied here too (confirmed as a real bug 2026-11-07: a vocal
    recording that was actually the right length still overshot by
    exactly the preamble's duration until this was added)."""
    sr = 22050
    data = _decode_mono_pcm(audio_path, skip_ms, sr)

    win = int(frame_ms / 1000 * sr)
    hop = int(hop_ms / 1000 * sr)
    rms_all = []
    midi_all = []
    for i in range(0, len(data) - win, hop):
        frame = data[i:i + win]
        rms = float(np.sqrt(np.mean(frame ** 2)))
        rms_all.append(rms)
        f0, clarity = _autocorrelation_f0(frame, sr)
        midi_all.append(None if (f0 <= 0 or clarity < min_clarity) else _hz_to_midi(f0))

    return _segment_pitch_track(rms_all, midi_all, hop_ms, silence_rms_ratio, min_note_ms)


_CREPE_MODEL_CACHE = {}  # model_size -> loaded weights, process-lifetime (loading is the slow part)


def from_acapella_ml(
    audio_path,
    skip_ms=0,
    hop_ms=10,
    min_note_ms=60,
    silence_rms_ratio=0.1,
    min_periodicity=0.5,
    model_size="tiny",
):
    """Monophonic pitch-tracking using torchcrepe (a neural pitch
    detector - CREPE, PyTorch CPU reimplementation), in place of
    from_acapella's autocorrelation heuristic. Confirmed 2026-11-14 on a
    real vocal recording: ~562MB peak RSS, ~55s for a 3.5-minute song on
    this VPS's CPU - run this through the SAME background queue +
    chrt/nice/ionice lowest-priority treatment as lyrics_sync.py's own
    ML mode, never inline on a request. Everything after per-frame pitch
    detection (silence thresholding, semitone rounding, smoothing,
    segmenting, short-fragment merging) is shared with from_acapella via
    _segment_pitch_track - only the detector itself differs, and this
    one is meaningfully more accurate (a real neural model trained for
    exactly this task, vs. a generic autocorrelation peak-pick), at the
    cost of a much heavier/slower dependency (PyTorch) - kept as a
    separate opt-in function rather than replacing from_acapella, so a
    caller that can't afford torch's footprint still has a working
    fallback."""
    import torch
    import torchcrepe

    sr = 16000
    data = _decode_mono_pcm(audio_path, skip_ms, sr)
    audio_t = torch.from_numpy(data.astype(np.float32)).unsqueeze(0)
    hop_length = round(hop_ms / 1000 * sr)

    pitch, periodicity = torchcrepe.predict(
        audio_t, sr, hop_length, fmin=50, fmax=1000, model=model_size,
        batch_size=512, device="cpu", return_periodicity=True,
    )
    pitch = pitch[0].numpy()
    periodicity = periodicity[0].numpy()

    win = round(40 / 1000 * sr)  # same 40ms RMS window from_acapella uses, for a consistent silence_rms_ratio meaning
    rms_all = []
    for i in range(len(pitch)):
        frame = data[i * hop_length:i * hop_length + win]
        rms_all.append(float(np.sqrt(np.mean(frame ** 2))) if len(frame) else 0.0)
    midi_all = [
        None if (hz <= 0 or per < min_periodicity) else _hz_to_midi(hz)
        for hz, per in zip(pitch, periodicity)
    ]

    return _segment_pitch_track(rms_all, midi_all, hop_ms, silence_rms_ratio, min_note_ms)
