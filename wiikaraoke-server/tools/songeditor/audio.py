"""
Streamed TPSA audio section — see project_wk_audio_streaming_format.md for
the full story. Confirmed on real hardware this session: a server-delivered
custom song's audio MUST be TPSA (never 4PMA, which is the real-disc/bundled
format and hangs the game with no completion report if used for a streamed
song), structured as:

  "TPSA" + 3 x u32 BE (len(raw_adts), count=1, flags=0) + 8 x u32 BE
  (w0=0, len(raw_adts)+32, w2=0, ADTS_FRAME_COUNT, 0, 0, 0, 0)   [48 bytes]
  then raw_adts split into 0x8000-byte chunks, each preceded by a 16-byte
  little-endian packet header: (chunk_len, 0, running_byte_offset, 0).

ADTS_FRAME_COUNT matters: the game's streaming-mode decoder only keeps
decoding "while fmt.12 > frame index" (fmt.12 is loaded from this exact
word) — get it wrong (left at 0, or copied from a differently-sized clip)
and playback caps out partway through, going silent, while the song's own
scoring/performance clock keeps running regardless (so "the performance
finished" does NOT mean "the audio played correctly" — a completely
separate failure mode from a too-short melody track hitting the
MSG_TV_NOTSCORE_SCORE_ERROR threshold; see project_wii_karaoke.md).
"""
import struct
import subprocess

CHUNK_SIZE = 0x8000
SAMPLES_PER_FRAME = 1024
SAMPLE_RATE = 44100


def count_adts_frames(raw_adts):
    """Walks the ADTS stream and returns its frame count. Raises if the
    walk doesn't land exactly on the end of the buffer — that means it
    isn't clean, gap-free ADTS (e.g. has trailing non-audio padding) and
    needs investigating before packaging."""
    p = 0
    frames = 0
    n = len(raw_adts)
    while p < n:
        assert raw_adts[p] == 0xFF and (raw_adts[p + 1] & 0xF0) == 0xF0, (
            "lost ADTS sync at byte %d" % p
        )
        frame_len = (
            ((raw_adts[p + 3] & 3) << 11) | (raw_adts[p + 4] << 3) | (raw_adts[p + 5] >> 5)
        )
        p += frame_len
        frames += 1
    assert p == n, "ADTS walk ended %d bytes short of the buffer" % (n - p)
    return frames


def trim_to_frame_boundary(raw_adts, target_ms):
    """Cuts raw_adts at the first frame boundary at or after target_ms.
    Returns (trimmed_bytes, actual_ms)."""
    p = 0
    frames = 0
    n = len(raw_adts)
    while p < n:
        frame_start_ms = frames * SAMPLES_PER_FRAME / SAMPLE_RATE * 1000
        if frame_start_ms >= target_ms:
            break
        frame_len = (
            ((raw_adts[p + 3] & 3) << 11) | (raw_adts[p + 4] << 3) | (raw_adts[p + 5] >> 5)
        )
        p += frame_len
        frames += 1
    actual_ms = frames * SAMPLES_PER_FRAME / SAMPLE_RATE * 1000
    return raw_adts[:p], actual_ms


def encode_to_adts(src_path, out_path, bitrate="128k"):
    """Re-encodes any ffmpeg-readable audio file to plain AAC-LC ADTS.

    -ar 44100 is not optional: every real song is 44100Hz (confirmed
    across 10 disc songs), and every duration/frame-boundary calculation
    in this module hardcodes SAMPLE_RATE=44100 to match. Without forcing
    it, ffmpeg just inherits whatever rate the source file happens to be
    at - a 48000Hz source WAV (common for "mastered" audio) went through
    silently, producing a real 48kHz ADTS stream our own round-trip
    checks couldn't catch (frame counting doesn't care about sample
    rate), while every one of our own ms calculations was quietly off by
    48000/44100, and the game's audio hardware - built around a fixed
    44.1kHz pipeline, matching every real song - froze on the loading
    screen initializing a stream at a rate it never expected.

    Real on-disc content averages only ~52-57kbps, and this used to be
    lowered to match on the (WRONG) theory that bitrate/file size was
    behind the loading-screen freeze. The actual cause was the UJK0
    body's section ORDER (audio must be last - see container.build's
    docstring and project_wk_audio_streaming_format.md), confirmed on
    hardware 2026-11-07, which has nothing to do with bitrate. Bitrate
    is a pure quality/size knob now - 128k (this default) is a safe,
    generous choice; raise it further if size isn't a concern.
    """
    subprocess.run(
        [
            "ffmpeg", "-v", "error", "-y", "-i", src_path,
            "-ar", str(SAMPLE_RATE),
            "-c:a", "aac", "-profile:a", "aac_low", "-b:a", bitrate,
            "-f", "adts", out_path,
        ],
        check=True,
    )
    probe = subprocess.run(
        ["ffprobe", "-v", "error", "-select_streams", "a:0",
         "-show_entries", "stream=sample_rate", "-of", "csv=p=0", out_path],
        capture_output=True, text=True, check=True,
    )
    actual_rate = int(probe.stdout.strip())
    if actual_rate != SAMPLE_RATE:
        raise RuntimeError(
            "encoded audio came out at %dHz, not %dHz - refusing to build "
            "a file whose timing math would be wrong" % (actual_rate, SAMPLE_RATE)
        )


def build_tpsa(raw_adts):
    """Builds a complete TPSA audio section from raw ADTS bytes."""
    frames = count_adts_frames(raw_adts)
    count, flags, w0, w2 = 1, 0, 0, 0
    header = b"TPSA" + struct.pack(">III", len(raw_adts), count, flags) + struct.pack(
        ">8I", w0, len(raw_adts) + 32, w2, frames, 0, 0, 0, 0
    )
    assert len(header) == 48
    out = bytearray(header)
    for o in range(0, len(raw_adts), CHUNK_SIZE):
        chunk = raw_adts[o:o + CHUNK_SIZE]
        out += struct.pack("<IIII", len(chunk), 0, o, 0) + chunk
    return bytes(out)


def unpack_tpsa(tpsa_section):
    """Reverses build_tpsa — mainly for self-verification. Returns raw_adts."""
    assert tpsa_section[:4] == b"TPSA"
    raw_len, _count, _flags = struct.unpack(">III", tpsa_section[4:16])
    dst = bytearray(raw_len)
    p = 48
    n = len(tpsa_section)
    while p < n:
        chunk_len, _z, offset, _tr = struct.unpack("<IIII", tpsa_section[p:p + 16])
        dst[offset:offset + chunk_len] = tpsa_section[p + 16:p + 16 + chunk_len]
        p += 16 + chunk_len
    assert p == n
    return bytes(dst)
