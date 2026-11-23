#!/usr/bin/env python3
"""
Build a minimal private main.bin: same approach as make_stub_db.py's
first_line_with_song(), but for wiiudb.dat's SONGS/ARTISTS tables, and with
every OTHER table emptied (not passed through full-size) — the real
German wiiudb.dat is 1.6MB and this keeps the private override small and
under our control, rather than re-shipping the entire disc catalog (which
is also the game's own, unmodified, possibly-stale data — see
project_wii_karaoke.md's 140-1114 catalog-error investigation).

  python3 tools/make_main_db.py <dump>/content/global/tsv/de/wiiudb.dat out.bin \
      <song_id> "<title>" <artist_id> "<artist name>"
"""
import struct
import sys

TABLE_HEADER_SIZE = 0x50
SONGS = 0xB3783E9E
ARTISTS = 0xA96E7E80


def _row(types, values):
    out = bytearray()
    strings = []
    for t, v in zip(types, values):
        if t == 1:
            strings.append(v)
            out += struct.pack(">I", 0)  # placeholder, patched below
        else:
            out += struct.pack(">I", v)
    return bytes(out), strings


def build(path, out_path, song_id, title, artist_id, artist_name):
    d = open(path, "rb").read()
    version, checksum, count = struct.unpack_from(">III", d, 0)
    entries, tables = [], []
    pos = 12 + count * 12
    for i in range(count):
        size, name, one = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        header = bytearray(d[start:start + TABLE_HEADER_SIZE])
        rowsize = header[0x2C]
        types = list(header[0x2D:0x2D + rowsize // 4])

        if name in (SONGS, ARTISTS):
            if name == SONGS:
                # Real-row shape confirmed from the EU/DE locale's own wiiudb.dat:
                # [id, title, artist_id(by value, not position), 2, year, 1, 1, 1,
                #  id(again), "", sort-letter, "0", 0, lyric-fk, lyric-fk] -
                # the last two are foreign keys into a per-line lyrics table we
                # don't have data for, so left at 0 (every other table is empty).
                values = [song_id, title, artist_id, 2, 2026, 1, 1, 1,
                          song_id, "", title[:1].upper(), "0", 0, 0, 0]
            else:
                values = [artist_id, artist_name, 8, 0, 0]
            assert len(values) == len(types), "%x row shape mismatch: %d types, %d values" % (
                name, len(types), len(values))
            row_header, strings = _row(types, values)
            row = bytearray(row_header)
            str_off = len(row)
            str_bytes = bytearray()
            si = 0
            for ci, t in enumerate(types):
                if t == 1:
                    off = str_off + len(str_bytes)
                    struct.pack_into(">I", row, ci * 4, off)
                    str_bytes += strings[si].encode("utf-8") + b"\0"
                    si += 1
            row += str_bytes
            row += b"\0" * (-len(row) % 16)
            table = bytearray(header) + row
            struct.pack_into(">II", table, 0x40, 1, len(row))
        else:
            table = header
            struct.pack_into(">II", table, 0x40, 0, 0)

        entries.append(struct.pack(">III", len(table), name, one))
        tables.append(bytes(table))
        pos = start + size

    out = bytearray(struct.pack(">III", version, checksum, count) + b"".join(entries))
    for table in tables:
        out += b"\0" * (-len(out) % 16) + table
    open(out_path, "wb").write(bytes(out))
    print("wrote", out_path, len(out), "bytes")


if __name__ == "__main__":
    if len(sys.argv) != 7:
        sys.exit(__doc__)
    build(sys.argv[1], sys.argv[2], int(sys.argv[3]), sys.argv[4], int(sys.argv[5]), sys.argv[6])
