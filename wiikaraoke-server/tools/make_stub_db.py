#!/usr/bin/env python3
"""
Write placeholder song, movie and first-line databases for GD0001/GM0001/GF0001, so the game gets past its
database download on a console with no songs. They contain no songs: only the table layout of the
game's own wiiudb.dat and song_begin_db.dat (see wiiudb.py), with every table emptied.

When a download finishes the game:
  1. XORs its first 1024 bytes with a 1024-byte table in .data (sub_021E6324); each kind of
     download has its own table (GD0001 0x10112268, GF0001 0x10113268, and probably GM0001 0x10113668)
  2. checks it (sub_0210E2A0): the last 32 bytes hold the little-endian crc32 of what precedes them

For GF0001 and GP0001 the game checks the download size minus 0x19000 (0x02050810), so those files
need that much padding behind the trailer. Without it the length underflows and the game crashes.

Everything is read from your own game dump, so none of it ends up in this repository.

  python3 tools/make_stub_db.py <dump with code/ and content/> content/db [private]
"""
import os
import struct
import sys
import zlib

TABLE_HEADER_SIZE = 0x50
FIRST_LINE_SONGS = 0x2658B880
TRAILER_SIZE = 32
XOR_TABLE_MAIN = 0x10112268
XOR_TABLE_FIRST_LINE = 0x10113268
XOR_TABLE_MOVIE = 0x10113668
XOR_SIZE = 1024
SHORT_CHECK_PADDING = 0x19000
SHF_RPL_ZLIB = 0x08000000
SHT_NOBITS = 8


def read_xor_table(rpx, va):
    d = open(rpx, "rb").read()
    shoff = struct.unpack(">I", d[0x20:0x24])[0]
    shnum = struct.unpack(">H", d[0x30:0x32])[0]
    for i in range(shnum):
        s = struct.unpack(">10I", d[shoff + i * 40:shoff + i * 40 + 40])
        flags, addr, off, size, typ = s[2], s[3], s[4], s[5], s[1]
        if typ == SHT_NOBITS or not addr:
            continue
        raw = d[off:off + size]
        if flags & SHF_RPL_ZLIB:
            raw = zlib.decompress(raw[4:])
        if addr <= va and va + XOR_SIZE <= addr + len(raw):
            return raw[va - addr:va - addr + XOR_SIZE]
    sys.exit("XOR table not found: is this the EU app.rpx?")


def xor_head(data, table):
    # Confirmed via live hardware memory scan: the game's XOR+CRC32 check
    # runs cyclically over the WHOLE received buffer, not just the first
    # XOR_SIZE bytes - the old first-chunk-only version here left anything
    # past byte 1024 unXORed, which the game then de-XORs into garbage.
    return bytes(b ^ table[i % XOR_SIZE] for i, b in enumerate(data))


def empty_tables(path):
    """The container from wiiudb.py with each table cut down to its 0x50-byte header and no rows."""
    d = open(path, "rb").read()
    version, checksum, count = struct.unpack_from(">III", d, 0)
    entries, tables = [], []
    pos = 12 + count * 12
    for i in range(count):
        size, name, one = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        header = bytearray(d[start:start + TABLE_HEADER_SIZE])
        struct.pack_into(">II", header, 0x40, 0, 0)
        entries.append(struct.pack(">III", TABLE_HEADER_SIZE, name, one))
        tables.append(bytes(header))
        pos = start + size
    out = bytearray(struct.pack(">III", version, checksum, count) + b"".join(entries))
    for table in tables:
        out += b"\0" * (-len(out) % 16) + table
    return bytes(out)


def first_line_with_song(path, song_id, text):
    """song_begin_db.dat with its song table holding one row: u32 song id, u32 text offset."""
    d = open(path, "rb").read()
    version, checksum, count = struct.unpack_from(">III", d, 0)
    entries, tables = [], []
    pos = 12 + count * 12
    for i in range(count):
        size, name, one = struct.unpack_from(">III", d, 12 + i * 12)
        start = (pos + 15) & ~15
        table = bytearray(d[start:start + size])
        if name == FIRST_LINE_SONGS:
            data = struct.pack(">II", song_id, 8) + text.encode() + b"\0"
            data += b"\0" * (-len(data) % 16)
            table = bytearray(d[start:start + TABLE_HEADER_SIZE]) + data
            struct.pack_into(">II", table, 0x40, 1, len(data))
        entries.append(struct.pack(">III", len(table), name, one))
        tables.append(bytes(table))
        pos = start + size
    out = bytearray(struct.pack(">III", version, checksum, count) + b"".join(entries))
    for table in tables:
        out += b"\0" * (-len(out) % 16) + table
    return bytes(out)


def stub(body, table):
    return xor_head(body + struct.pack("<I", zlib.crc32(body)) + b"\0" * (TRAILER_SIZE - 4), table)


def main(dump, out, private=None):
    rpx = os.path.join(dump, "code", "app.rpx")
    tsv = os.path.join(dump, "content", "global", "tsv", "en")
    main_db = empty_tables(os.path.join(tsv, "wiiudb.dat"))
    first_line = empty_tables(os.path.join(tsv, "song_begin_db.dat"))
    os.makedirs(out, exist_ok=True)
    for name, body, va, padding in (("main.bin", main_db, XOR_TABLE_MAIN, 0),
                                    ("movie.bin", main_db + b"movie", XOR_TABLE_MOVIE, 0),
                                    ("firstline.bin", first_line, XOR_TABLE_FIRST_LINE, SHORT_CHECK_PADDING)):
        with open(os.path.join(out, name), "wb") as f:
            f.write(stub(body, read_xor_table(rpx, va)) + b"\0" * padding)
        print("wrote", os.path.join(out, name))

    # With a private_dir argument: a first-line database with one placeholder entry, for testing.
    if private:
        os.makedirs(private, exist_ok=True)
        body = first_line_with_song(os.path.join(tsv, "song_begin_db.dat"), 750259, "Test song")
        with open(os.path.join(private, "firstline-test.bin"), "wb") as f:
            f.write(stub(body, read_xor_table(rpx, XOR_TABLE_FIRST_LINE)) + b"\0" * SHORT_CHECK_PADDING)

        # And the game's own full databases, for accounts testing with real song data.
        de = os.path.join(dump, "content", "global", "tsv", "de")
        with open(os.path.join(de, "wiiudb.dat"), "rb") as f:
            full = stub(f.read(), read_xor_table(rpx, XOR_TABLE_MAIN))
        with open(os.path.join(private, "main-full.bin"), "wb") as f:
            f.write(full)
        with open(os.path.join(de, "song_begin_db.dat"), "rb") as f:
            full = stub(f.read(), read_xor_table(rpx, XOR_TABLE_FIRST_LINE))
        with open(os.path.join(private, "firstline-full.bin"), "wb") as f:
            f.write(full + b"\0" * SHORT_CHECK_PADDING)


if __name__ == "__main__":
    if len(sys.argv) not in (3, 4):
        sys.exit(__doc__)
    main(*sys.argv[1:])
