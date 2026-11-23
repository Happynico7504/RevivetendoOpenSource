#!/usr/bin/env python3
"""
Build a properly-emptied wiiudb.dat with zero custom songs - the correct
starting point for add_custom_song.py to then append onto, one song at a
time, building up a from-scratch catalog that never carries any of the
original disc's 2658 songs/994 artists.

NOT the same as the old make_main_db.py (confirmed broken: it zeroed every
non-SONGS/ARTISTS table, including ones later confirmed required for
search/browse - TITLE_INDEX_TABLES, TIMESTAMP_TABLE, MISC_TABLE, and the
letter/prefix bucket-count tables add_custom_song.py's own fixes depend
on). This empties exactly the song/artist-COUNT-dependent tables (to 0
rows, or to the same row count with every count reset to 0 for the
fixed-size letter buckets) and leaves every other table - genre names,
year ranges, the 9 artist-keyed tables not needed for search/playback,
and anything else whose meaning isn't pinned down - untouched, copied
verbatim from the source dump.

Why build from scratch at all, instead of appending onto the real 2658
real songs: we don't have the actual audio/lyric content for any of the
original licensed songs anyway (never captured, only their catalog
metadata survived in the dump), and mixing new rows into a catalog this
size and this interlinked (title_rank/artist_rank permutations, 3
single-letter + 2 two-letter bucket-count tables, 9 parallel sort-order
tables) kept surfacing one more undocumented invariant every time one was
fixed - up to and including a scrollbar-touch crash that reproduces on
the modified catalog but not on the stock one, suggesting SOMETHING about
exceeding the disc's original row counts breaks a part of the game's own
UI code we haven't found yet. Starting from zero sidesteps all of it: our
own small catalog never approaches whatever that limit is, and every
rank/bucket/count becomes trivial (insert position 0, append-only) by
construction, which add_custom_song.py's existing logic already handles
correctly without any changes.

  python3 tools/make_empty_stub.py <dump>/content/global/tsv/de/wiiudb.dat out.bin
"""
import struct
import sys

TABLE_HEADER_SIZE = 0x50

ZERO_ROWS_TABLES = {
    0xB3783E9E,  # SONGS
    0xA96E7E80,  # ARTISTS
    0x2447936C, 0x27100A6D, 0x04940240, 0x9B9CE8D9,  # TITLE_INDEX_TABLES
    0x7A824535, 0xBF60595E, 0x3717A9CE, 0x5E5B1666, 0x2EF39EDD,
    0x583D5F55,  # TIMESTAMP_TABLE
    0xC4E8EC95,  # MISC_TABLE
    0x5F39CFFA, 0x8F4C63E9,  # SONGS_TITLE/ARTIST_PREFIX_COUNT (2-letter)

    # CONFIRMED (checked every value in every untouched table against the
    # real SONGS/ARTISTS id sets): every one of these has EVERY row - not
    # just some - referencing a real stock song_id or artist_id that
    # doesn't exist once the catalog is this small. Most likely cause of
    # the "crashes on catalog open" symptom (letter-jump/scroll fixes
    # didn't touch these at all, since they were never flagged as
    # song/artist-count-dependent before this check). 8 of these are
    # per-category/genre song-rank lists ([1900-flag, rank, song_id],
    # varying row counts 222-1133 - clearly subset lists, not full-catalog
    # tables); the rest are full song-keyed or artist-keyed parallel
    # tables at exactly 2658 or 994 rows, same shape as the already-known
    # 11 song-keyed + 9 artist-keyed tables, just not referenced by
    # add_custom_song.py's existing per-row append logic since nothing
    # ever needed to grow them before.
    0xA9F77040, 0x8A73786D, 0x157B92F4, 0xF4653F18,  # per-category song-rank lists
    0x31872373, 0xB9F0D3E3, 0xD0BC6C4B, 0xA014E4F0,
    0x1216FBD5, 0x12316887, 0x15D7815F, 0x14FD435C,  # song-keyed (2823/4443/1517 rows)
    0x3E51D372, 0xE8BD90EC, 0x20B8B391, 0x3D0C4F7D,  # artist-keyed (994/1102/1935 rows)
    0xD7E08BE9,
    0x35057E59, 0x16817674, 0x89899CED, 0x68973101,  # 8 more artist-keyed (994 rows each)
    0xAD752D6A, 0x2502DDFA, 0x4C4E6252, 0x3CE6EAE9,
}
# [letter, count] tables - keep all 37 real letter rows, just zero every count.
RESET_COUNT_TABLES = {
    0x88683FC2,  # SONGS_TITLE_LETTER_COUNT
    0x6C48405A,  # SONGS_ARTIST_LETTER_COUNT
    0xA9CEDE03,  # ARTISTS_LETTER_COUNT
    0x10736E87,  # unused 4th bucket (already all-zero in the real data)
}
# Single fixed row of song_id slots - keep the row, clear every slot to 0.
ZERO_SLOTS_TABLES = {
    0x4B0224EA,  # FEATURED_SONGS_TABLE
    0x91379A15,  # HIGHLIGHTED_SONGS_TABLE
}


def build(path, out_path):
    d = open(path, "rb").read()
    version, checksum, count = struct.unpack_from(">III", d, 0)
    entries, tables = [], []
    pos = 12 + count * 12
    for i in range(count):
        size, name, one = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        table = bytearray(d[start:start + size])
        rowsize = table[0x2C]

        if name in ZERO_ROWS_TABLES:
            table = bytearray(table[:TABLE_HEADER_SIZE])
            struct.pack_into(">II", table, 0x40, 0, 0)
        elif name in RESET_COUNT_TABLES:
            rows = struct.unpack_from(">I", table, 0x40)[0]
            for r in range(rows):
                off = TABLE_HEADER_SIZE + r * rowsize + 4  # column 1 = the count
                struct.pack_into(">I", table, off, 0)
        elif name in ZERO_SLOTS_TABLES:
            rows = struct.unpack_from(">I", table, 0x40)[0]
            assert rows == 1, "unexpected row count for a featured/highlighted table"
            for ci in range(rowsize // 4):
                struct.pack_into(">I", table, TABLE_HEADER_SIZE + ci * 4, 0)
        # every other table: pass through unmodified, byte-for-byte

        entries.append(struct.pack(">III", len(table), name, one))
        tables.append(bytes(table))
        pos = start + size

    out = bytearray(struct.pack(">III", version, checksum, count) + b"".join(entries))
    for table in tables:
        out += b"\0" * (-len(out) % 16) + table
    open(out_path, "wb").write(bytes(out))
    print("wrote", out_path, len(out), "bytes")


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    build(sys.argv[1], sys.argv[2])
