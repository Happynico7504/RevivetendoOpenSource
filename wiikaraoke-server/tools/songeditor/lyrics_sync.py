"""
Three ways to produce the (text, start_ms, end_ms) lyric line list that
song.Song's lyrics_lines expects:

  from_manual(entries)        — you specify every line's own timing.
  from_lrc(lrc_text)          — import a standard .lrc synced-lyrics file.
  from_acapella(lines, path)  — paste plain lyric text (no timestamps) and
                                 auto-sync it against an isolated vocal
                                 recording's voice-activity (silence vs
                                 singing), one detected phrase per line.

Mirrors notes.py's from_manual/from_midi/from_acapella split for the
melody - same reasoning: manual is exact but tedious, the automatic modes
are best-effort reductions that always deserve a manual touch-up pass
afterward, never a guaranteed-correct final answer.
"""
import bisect
import difflib
import re
import subprocess

import numpy as np

_LRC_TAG = re.compile(r"\[(\d+):(\d+(?:\.\d+)?)\]")

# how long a trailing line (or the only line) is shown for when nothing
# else determines its end_ms - matches a typical sung line's length,
# not meant to be precise.
DEFAULT_LINE_MS = 4000


# --- manual mode ---

def snap_to_notes(lyrics_lines, note_list, preamble_ms, max_snap_ms=500):
    """Snaps each lyric line's (start_ms, end_ms) - already shifted to
    track-absolute time, same convention as everywhere else in this
    module - to the nearest note boundary in note_list's own cumulative
    timeline, instead of leaving them at whatever independently-computed
    real-time value produced them.

    Confirmed 2026-11-14 (reported as lyrics timing still drifting a
    bit after every other fix that session): notes and lyrics are
    extracted by two entirely separate pipelines against the same real
    audio (torchcrepe/autocorrelation pitch-segmentation for notes,
    Whisper word-alignment for lyrics), and even though both are
    "correct" against real time independently, their own segmentation
    boundaries don't land on exactly the same instants - and the
    engine's own internal clock is driven by walking through the notes
    sequence (see Song.total_ms()'s own 2026-11-14 fix for why that's
    now known, not assumed), not by matching wall-clock time on its own.
    Snapping lyrics onto the SAME reference the notes themselves define
    removes that drift at its source, rather than just reducing it -
    a lyric line now always starts/ends exactly when some real note in
    the track does, never at a timestamp the track's own pacing has no
    notion of.

    max_snap_ms bounds this: confirmed 2026-11-14 that during a long
    rest (the ML note detector missing pitch across a real singer's
    pause - several real seconds with no note boundary at all), the
    nearest boundary can be seconds away, and snapping onto it
    unconditionally makes that line's timing WORSE than leaving its own
    independently-computed timestamp alone. Past max_snap_ms, the
    original timestamp is kept untouched rather than forced onto a
    distant boundary - this fix is meant to remove sub-boundary jitter
    between the two pipelines, not override one with the other when
    they disagree by seconds."""
    boundaries = [preamble_ms]
    for duration_ms, _pitch in note_list:
        boundaries.append(boundaries[-1] + duration_ms)

    def snap(ms):
        i = bisect.bisect_right(boundaries, ms) - 1
        i = max(0, min(i, len(boundaries) - 1))
        if i + 1 < len(boundaries) and abs(boundaries[i + 1] - ms) < abs(boundaries[i] - ms):
            i += 1
        if abs(boundaries[i] - ms) > max_snap_ms:
            return ms
        return boundaries[i]

    out = []
    for text, start_ms, end_ms in lyrics_lines:
        s, e = snap(start_ms), snap(end_ms)
        if e <= s:
            e = s + 1
        out.append((text, s, e))
    return out


def enforce_min_display(lyrics_lines, min_floor_ms=1200, ms_per_char=130):
    """Pushes each line's start_ms (and end_ms by the same amount, to
    keep its own duration) forward if the PREVIOUS line wouldn't have
    been on screen long enough - at least max(min_floor_ms, ms_per_char
    * len(text)), not just "at least until its own end_ms".

    Confirmed 2026-11-14 (reported on real hardware as lines changing
    before the words finish, every time, not growing worse over the
    song - ruled out as cumulative drift): since lines display SHOW-
    only (lyrics.show_hide_events's emit_hide=False - a line stays
    visible until the NEXT line's own SHOW event, never cleared by its
    own end_ms), a line's actual on-screen duration is entirely
    determined by the NEXT line's start_ms, regardless of what its own
    end_ms says.

    A plain "next line can't start before this one's own end_ms" (the
    first version of this function, same day) turned out insufficient:
    checked against a real build and found zero such overlaps, yet the
    hardware symptom persisted - meaning Whisper's word-level end_ms
    ITSELF is too tight for sung (vs spoken) content to use as the
    floor. Whisper is trained on speech, not singing - a sustained/held
    sung syllable runs longer in the real recording than its
    ASR-estimated word boundary. A length-scaled floor (clearly
    heuristic, not measured from this recording) is a more realistic
    minimum than trusting the ASR boundary outright. Applied after
    snap_to_notes, since snapping itself (even bounded by max_snap_ms)
    could reintroduce the same too-short-display problem."""
    out = [lyrics_lines[0]] if lyrics_lines else []
    for text, start_ms, end_ms in lyrics_lines[1:]:
        prev_text, prev_start, prev_end = out[-1]
        # max(), not just the floor alone: prev_end may already be later
        # than prev_start + this floor (its own natural/shifted duration
        # already exceeds the heuristic minimum), and a shorter floor
        # must never pull the next line's start back inside that window.
        min_end = max(prev_end, prev_start + max(min_floor_ms, ms_per_char * len(prev_text)))
        if start_ms < min_end:
            shift = min_end - start_ms
            start_ms, end_ms = min_end, end_ms + shift
        out.append((text, start_ms, end_ms))
    return out


def from_manual(entries):
    """entries: list of (text, start_ms, end_ms)."""
    out = []
    for text, start_ms, end_ms in entries:
        if end_ms <= start_ms:
            raise ValueError("line %r: end_ms must be after start_ms" % (text,))
        out.append((text, int(start_ms), int(end_ms)))
    return out


# --- .lrc upload mode ---

def from_lrc(lrc_text, default_line_ms=DEFAULT_LINE_MS):
    """Parses a standard .lrc file: one or more [mm:ss.xx] tags per line,
    each followed by that line's text (a line with multiple tags, e.g.
    for a chorus repeated at several timestamps, expands to one entry
    per tag - standard .lrc behavior). Metadata tags ([ti:...], [ar:...]
    etc - anything whose "mm" part isn't purely digits-colon-digits
    timing) and blank lines are ignored. end_ms for each line is the
    next line's own start_ms; the last line gets default_line_ms."""
    entries = []  # (start_ms, text)
    for raw_line in lrc_text.splitlines():
        tags = list(_LRC_TAG.finditer(raw_line))
        if not tags:
            continue
        text = _LRC_TAG.sub("", raw_line).strip()
        if not text:
            continue
        for m in tags:
            minutes, seconds = int(m.group(1)), float(m.group(2))
            start_ms = round((minutes * 60 + seconds) * 1000)
            entries.append((start_ms, text))
    entries.sort(key=lambda e: e[0])
    out = []
    for i, (start_ms, text) in enumerate(entries):
        end_ms = entries[i + 1][0] if i + 1 < len(entries) else start_ms + default_line_ms
        if end_ms <= start_ms:
            end_ms = start_ms + 1
        out.append((text, start_ms, end_ms))
    return out


# --- a cappella auto-sync mode ---

_WORD_RE = re.compile(r"[a-z0-9']+")
_WHISPER_MODEL = None  # lazily loaded, process-lifetime singleton - see from_acapella


def _normalize_words(text):
    return _WORD_RE.findall(text.lower())


def _decode_audio_f32(audio_path, skip_ms, sr=16000):
    """Decodes to mono float32 PCM at Whisper's own expected sample rate via
    ffmpeg (same ffmpeg-subprocess pattern every other audio-reading
    function in this package already uses), rather than letting faster-
    whisper's own PyAV-based decode_audio() touch the file directly - a
    version mismatch between faster-whisper 1.2.1 and a newer PyAV release
    broke that path outright (av.open() rejecting a kwarg faster-whisper
    still passes), confirmed 2026-11-14."""
    proc = subprocess.run(
        ["ffmpeg", "-v", "error", "-y", "-ss", str(skip_ms / 1000), "-i", audio_path,
         "-ar", str(sr), "-ac", "1", "-f", "f32le", "-"],
        capture_output=True, check=True,
    )
    return np.frombuffer(proc.stdout, dtype=np.float32)


def _get_whisper_model(model_size):
    """Process-lifetime singleton - loading the model is the slow/memory
    part (confirmed ~1-3s and ~250MB RSS for "tiny" int8 2026-11-14), so a
    long-running worker process should load it once and reuse it across
    jobs, not reload per request."""
    global _WHISPER_MODEL
    if _WHISPER_MODEL is None:
        from faster_whisper import WhisperModel
        _WHISPER_MODEL = WhisperModel(model_size, device="cpu", compute_type="int8")
    return _WHISPER_MODEL


def from_acapella(lines, audio_path, skip_ms=0, model_size="tiny"):
    """lines: plain text, no timestamps - one entry per lyric line, in
    singing order. Runs a local, CPU-only speech-recognition model
    (faster-whisper, int8-quantized "tiny" by default - confirmed
    2026-11-14 to use a modest ~325MB peak RSS and take ~24s for a
    3.5-minute song on this VPS's 4-core/4GB box, well within what's
    available even with the live game servers also running; see
    songeditor_build.go/the lyrics-sync worker for why this still needs
    to run at the lowest CPU priority and one job at a time regardless)
    to get word-level timestamps, then aligns the ASR's own (imperfect,
    often differently-worded/punctuated) transcription against the
    caller's actual lyric text via difflib's longest-matching-blocks
    algorithm - the same technique text diffing uses, applied here to
    find which ASR words correspond to which of the caller's own words
    rather than trusting the ASR's transcribed TEXT at all, only its
    TIMING. A line gets its timing from the first and last ASR word
    matched to it; a line with zero matches (the ASR missed or badly
    mis-heard it) is interpolated linearly between its nearest matched
    neighbors, or spread evenly across the whole track if nothing at
    all matched. Confirmed 2026-11-14 recognizing real sung lyrics
    correctly enough for this to work ("Red Shells chasing, Green
    Shells taunting" from a real, not-spoken-clearly vocal line) - still
    best-effort like every other automatic mode here, worth a manual
    touch-up pass, especially for any interpolated (zero-match) lines."""
    if not lines:
        raise ValueError("no lines given")

    audio = _decode_audio_f32(audio_path, skip_ms)
    model = _get_whisper_model(model_size)
    segments, _info = model.transcribe(audio, word_timestamps=True)
    asr_words = []  # (normalized_word, start_s, end_s)
    for seg in segments:
        for w in seg.words:
            for nw in _normalize_words(w.word):
                asr_words.append((nw, w.start, w.end))
    if not asr_words:
        raise ValueError("the a cappella recording produced no recognizable speech")

    line_word_counts = [len(_normalize_words(t)) for t in lines]
    user_words = []  # (line_index, normalized_word)
    for i, text in enumerate(lines):
        for w in _normalize_words(text):
            user_words.append((i, w))

    asr_seq = [w[0] for w in asr_words]
    user_seq = [w[1] for w in user_words]
    matcher = difflib.SequenceMatcher(a=asr_seq, b=user_seq, autojunk=False)

    line_spans = [None] * len(lines)  # [min_start_s, max_end_s] per line
    for asr_i, user_j, size in matcher.get_matching_blocks():
        for k in range(size):
            asr_start_s, asr_end_s = asr_words[asr_i + k][1], asr_words[asr_i + k][2]
            line_idx = user_words[user_j + k][0]
            if line_spans[line_idx] is None:
                line_spans[line_idx] = [asr_start_s, asr_end_s]
            else:
                line_spans[line_idx][0] = min(line_spans[line_idx][0], asr_start_s)
                line_spans[line_idx][1] = max(line_spans[line_idx][1], asr_end_s)

    track_end_s = asr_words[-1][2]
    _interpolate_missing_spans(line_spans, track_end_s)

    return [(text, round(span[0] * 1000) + skip_ms, round(span[1] * 1000) + skip_ms)
            for text, span in zip(lines, line_spans)]


def _interpolate_missing_spans(line_spans, track_end_s):
    """Fills in any None entries (lines the ASR alignment found zero
    matching words for) by linear interpolation between the nearest
    matched neighbors on either side - or, if NOTHING at all matched,
    spreads every line evenly across the whole track as an absolute
    last resort."""
    matched = [i for i, s in enumerate(line_spans) if s is not None]
    if not matched:
        n = len(line_spans)
        for i in range(n):
            line_spans[i] = [track_end_s * i / n, track_end_s * (i + 1) / n]
        return
    for i in range(len(line_spans)):
        if line_spans[i] is not None:
            continue
        prev_i = max((m for m in matched if m < i), default=None)
        next_i = min((m for m in matched if m > i), default=None)
        prev_end = line_spans[prev_i][1] if prev_i is not None else 0.0
        next_start = line_spans[next_i][0] if next_i is not None else track_end_s
        # evenly split the gap among every consecutive unmatched line in it
        gap_start = next((j for j in range(i, -1, -1) if line_spans[j] is not None), -1) + 1
        gap_end = next((j for j in range(i, len(line_spans)) if line_spans[j] is not None), len(line_spans))
        gap_len = gap_end - gap_start
        pos = i - gap_start
        step = (next_start - prev_end) / (gap_len + 1)
        line_spans[i] = [prev_end + step * pos, prev_end + step * (pos + 1)]


def _voice_activity(audio_path, skip_ms, frame_ms, hop_ms, silence_rms_ratio):
    import tempfile
    import wave

    sr = 22050
    with tempfile.NamedTemporaryFile(suffix=".wav") as tmp:
        subprocess.run(
            ["ffmpeg", "-v", "error", "-y", "-ss", str(skip_ms / 1000), "-i", audio_path,
             "-ar", str(sr), "-ac", "1", tmp.name],
            check=True,
        )
        w = wave.open(tmp.name, "rb")
        n = w.getnframes()
        data = np.frombuffer(w.readframes(n), dtype=np.int16).astype(np.float64)

    win = int(frame_ms / 1000 * sr)
    hop = int(hop_ms / 1000 * sr)
    rms = np.array([
        float(np.sqrt(np.mean(data[i:i + win] ** 2)))
        for i in range(0, max(1, len(data) - win), hop)
    ])
    threshold = (np.median(rms[rms > 0]) if np.any(rms > 0) else 0.0) * silence_rms_ratio
    return rms > threshold, hop_ms


def from_acapella_vad(lines, audio_path, skip_ms=0, frame_ms=40, hop_ms=10,
                       silence_rms_ratio=0.1, min_gap_ms=300):
    """lines: plain text, no timestamps - one entry per lyric line, in
    singing order. The ORIGINAL a cappella sync approach (superseded by
    from_acapella's ML-based alignment above, kept as a fallback in case
    the ML model is ever unavailable) - finds voiced (singing) vs silent
    frames via RMS-vs-threshold (same tuning as notes.from_acapella),
    merges voiced runs separated by gaps shorter than min_gap_ms (a
    breath pause mid-line shouldn't split it into two phrases), then
    matches the resulting phrases to lines 1:1 in order.

    Best-effort, same spirit as notes.from_acapella: if more phrases are
    found than lines, the shortest gaps are merged away until the counts
    match; if fewer are found, the few phrases found are split
    proportionally by each line's own character count (a rough stand-in
    for syllable count) rather than left unmatched. Either fallback
    means the result deserves a manual touch-up pass, same as notes'
    own acapella mode - this is not a guaranteed-correct final answer,
    only a reasonable starting point."""
    if not lines:
        raise ValueError("no lines given")

    voiced, hop_ms = _voice_activity(audio_path, skip_ms, frame_ms, hop_ms, silence_rms_ratio)
    phrases = []  # [start_frame, end_frame] inclusive-exclusive
    i = 0
    n = len(voiced)
    while i < n:
        if voiced[i]:
            start = i
            while i < n and voiced[i]:
                i += 1
            phrases.append([start, i])
        else:
            i += 1

    min_gap_frames = max(1, round(min_gap_ms / hop_ms))
    merged = []
    for p in phrases:
        if merged and p[0] - merged[-1][1] < min_gap_frames:
            merged[-1][1] = p[1]
        else:
            merged.append(p)
    phrases = merged

    if not phrases:
        raise ValueError("no voiced (singing) audio detected - check the recording or lower silence_rms_ratio")

    # merge the smallest gaps until phrase count matches line count
    while len(phrases) > len(lines):
        gaps = [phrases[i + 1][0] - phrases[i][1] for i in range(len(phrases) - 1)]
        j = int(np.argmin(gaps))
        phrases[j][1] = phrases[j + 1][1]
        del phrases[j + 1]

    if len(phrases) < len(lines):
        # split the phrases we DO have, proportionally by character
        # count across however many lines map onto each one - a rough
        # stand-in for syllable density, not a real alignment.
        out = []
        lines_per_phrase = np.array_split(np.arange(len(lines)), len(phrases))
        for phrase, idxs in zip(phrases, lines_per_phrase):
            start_ms = phrase[0] * hop_ms + skip_ms
            end_ms = phrase[1] * hop_ms + skip_ms
            sub_lines = [lines[k] for k in idxs]
            weights = np.array([max(1, len(t)) for t in sub_lines], dtype=float)
            weights /= weights.sum()
            cum = np.concatenate([[0], np.cumsum(weights)])
            for w_i, k in enumerate(idxs):
                s = start_ms + (end_ms - start_ms) * cum[w_i]
                e = start_ms + (end_ms - start_ms) * cum[w_i + 1]
                out.append((lines[k], round(s), round(e)))
        return out

    return [(text, phrase[0] * hop_ms + skip_ms, phrase[1] * hop_ms + skip_ms)
            for text, phrase in zip(lines, phrases)]
