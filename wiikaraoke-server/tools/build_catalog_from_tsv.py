#!/usr/bin/env python3
"""
Build the complete custom-songs wiiudb.dat catalog from a single TSV, in one
pass - the deployment-pipeline-friendly alternative to running
add_custom_song.py by hand once per song against whatever the previous
output was.

Reuses make_empty_stub.build() (zero real Nintendo songs/artists - see its
own docstring for why: we don't have the original audio/lyrics anyway, and
every table add_custom_song.py's own append logic depends on becomes
trivial to keep correct when it's built up from empty) and
add_custom_song.build() (the actual per-row table logic: SONGS/ARTISTS/the
9 TITLE_INDEX_TABLES/TIMESTAMP_TABLE/MISC_TABLE, the 3 single-letter + 2
two-letter bucket-count tables, and the title_rank/artist_rank/ARTISTS.col3
alphabetical-position inserts) unchanged - this is just the driver loop.

Sorting the TSV alphabetically by title first (not required for
correctness - add_custom_song.py's bisect-based rank insertion places each
row correctly regardless of input order - but it keeps the source file
itself readable/reviewable in the same order the in-game list will show
them, and makes a diff of the TSV a meaningful preview of the in-game
reorder).

TSV columns (tab-separated, no header row): song_id, title, artist_name
One row per song. The same artist_name reused across multiple rows shares
one ARTISTS entry (and one artist_id, derived deterministically from the
name so repeat runs are stable) - it is only inserted into ARTISTS once,
on its first occurrence.

template_path can be either a real plain (unwrapped) wiiudb.dat (used only
for table structure/headers - gets emptied first) or an already-empty
stub such as the committed tools/wiiudb_empty_stub.bin (detected via
SONGS' own row count and used as-is, no real dump needed at all for the
normal case).

  python3 tools/build_catalog_from_tsv.py songs.tsv tools/wiiudb_empty_stub.bin out.bin
"""
import argparse
import csv
import hashlib
import os
import struct
import sys
import zlib

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import add_custom_song
import make_empty_stub

SONGS = 0xB3783E9E
FEATURED_SONGS_TABLE = 0x4B0224EA
HIGHLIGHTED_SONGS_TABLE = 0x91379A15
TABLE_HEADER_SIZE = 0x50


def fill_featured_slots(path, song_ids):
    """Final pass over FEATURED_SONGS_TABLE/HIGHLIGHTED_SONGS_TABLE,
    overwriting whatever add_custom_song.py's own per-row "fill the first
    zero slot" calls left behind (correct for a 1:1 real-song-count match,
    wrong otherwise - see add_custom_song.py's comment on this table).
    CONFIRMED against the real, unmodified stock data: both tables always
    carry EXACTLY ONE zero slot, at no particular position (HIGHLIGHTED's
    was slot 0, not the last one) - not a "real entries then a
    terminator" structure, just "every slot filled except exactly one,
    anywhere". Cycles through song_ids (repeating if there are fewer
    songs than slots-1, which is the normal case for a small custom
    catalog) to fill every slot but one, which is explicitly zeroed."""
    if not song_ids:
        return
    d = bytearray(open(path, "rb").read())
    _, _, count = struct.unpack_from(">III", d, 0)
    pos = 12 + count * 12
    for i in range(count):
        size, name, _ = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        if name in (FEATURED_SONGS_TABLE, HIGHLIGHTED_SONGS_TABLE):
            rowsize = d[start + 0x2C]
            n_slots = rowsize // 4
            for ci in range(n_slots):
                off = start + TABLE_HEADER_SIZE + ci * 4
                # Zero slot at index 0, matching the real stock data's own
                # HIGHLIGHTED_SONGS_TABLE convention exactly (confirmed:
                # its zero sat at slot 0, not the last slot).
                value = 0 if ci == 0 else song_ids[(ci - 1) % len(song_ids)]
                struct.pack_into(">I", d, off, value)
        pos = start + size
    # Re-stamp the whole-file trailer CRC (see add_custom_song.py's own
    # end-of-build comment for the format) - stale otherwise, since this
    # patches bytes after the last add_custom_song.build() call already
    # wrote one for the pre-patch content.
    crc = zlib.crc32(bytes(d[:-32])) & 0xFFFFFFFF
    swapped = struct.unpack("<I", struct.pack(">I", crc))[0]
    struct.pack_into(">I", d, len(d) - 32, swapped)
    open(path, "wb").write(bytes(d))


def _is_already_empty(path):
    d = open(path, "rb").read()
    _, _, count = struct.unpack_from(">III", d, 0)
    pos = 12 + count * 12
    for i in range(count):
        size, name, _ = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        if name == SONGS:
            return struct.unpack_from(">I", d, start + 0x40)[0] == 0
        pos = start + size
    return False

ARTIST_ID_BASE = 9100000  # separate range from song ids (9000001+) - avoids collisions


def artist_id_for(name, seen):
    """Deterministic id for a given artist name, stable across runs (so a
    rebuild with the same TSV reproduces the same bytes) - derived from the
    name's own hash rather than assignment order, so inserting a new artist
    earlier in the TSV doesn't shift every later artist's id."""
    if name in seen:
        return seen[name]
    h = int(hashlib.md5(name.encode("utf-8")).hexdigest(), 16)
    artist_id = ARTIST_ID_BASE + (h % 900000)
    seen[name] = artist_id
    return artist_id


def build(tsv_path, template_path, out_path):
    rows = []
    with open(tsv_path, newline="", encoding="utf-8") as f:
        for line in csv.reader(f, delimiter="\t"):
            if not line or line[0].startswith("#"):
                continue
            song_id, title, artist_name = line[0], line[1], line[2]
            rows.append((int(song_id), title, artist_name))

    rows.sort(key=lambda r: add_custom_song._sort_key(r[1]))

    if _is_already_empty(template_path):
        stub_path = template_path
    else:
        stub_path = out_path + ".stub"
        make_empty_stub.build(template_path, stub_path)

    seen_artists = {}
    inserted_artists = set()
    current = stub_path
    for i, (song_id, title, artist_name) in enumerate(rows):
        artist_id = artist_id_for(artist_name, seen_artists)
        next_path = out_path + f".step{i}"
        add_custom_song.build(
            current, next_path, song_id, title, artist_id,
            artist_name if artist_name not in inserted_artists else None,
        )
        inserted_artists.add(artist_name)
        if current != stub_path:
            os.remove(current)
        current = next_path

    os.replace(current, out_path)
    if stub_path != template_path:
        os.remove(stub_path)
    fill_featured_slots(out_path, [song_id for song_id, _, _ in rows])
    print("wrote", out_path, "from", len(rows), "song(s),", len(seen_artists), "artist(s)")


if __name__ == "__main__":
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("tsv_path")
    ap.add_argument("template_path", help="any real plain (unwrapped) wiiudb.dat, used only for table structure/headers")
    ap.add_argument("out_path")
    args = ap.parse_args()
    build(args.tsv_path, args.template_path, args.out_path)
