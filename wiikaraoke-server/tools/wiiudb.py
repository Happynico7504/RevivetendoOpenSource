#!/usr/bin/env python3
"""
Reader for Wii Karaoke U's song database (content/global/tsv/<lang>/wiiudb.dat).

Container (big endian):  u32 version, u32 checksum?, u32 table count, then
count x {u32 size, u32 name hash, u32 1}; tables follow, each 16-byte aligned.
Table: u32 ?, u16 column widths[...] (@0x04), u8 row size (@0x2C), u8 column
types[...] (@0x2D, 3 = u32, 1 = string offset), u32 row count (@0x40),
u32 data size (@0x44), rows at 0x50; string offsets are relative to 0x50.

  python3 tools/wiiudb.py wiiudb.dat            # list tables
  python3 tools/wiiudb.py wiiudb.dat songs      # dump song list (id, title, artist)
"""
import struct
import sys

SONGS = 0xB3783E9E
ARTISTS = 0xA96E7E80


class Table:
    def __init__(self, blk):
        self.blk = blk
        self.rowsize = blk[0x2C]
        n = self.rowsize // 4
        self.types = list(blk[0x2D:0x2D + n])
        self.rows = struct.unpack_from(">I", blk, 0x40)[0]

    def cell_str(self, off):
        p = 0x50 + off
        e = self.blk.index(b"\0", p)
        return self.blk[p:e].decode("utf-8", "replace")

    def row(self, i):
        vals = struct.unpack_from(">%dI" % (self.rowsize // 4), self.blk, 0x50 + i * self.rowsize)
        return [self.cell_str(v) if t == 1 else v for v, t in zip(vals, self.types)]

    def __iter__(self):
        return (self.row(i) for i in range(self.rows))


class WiiUDB:
    def __init__(self, path):
        d = open(path, "rb").read()
        self.version, _, n = struct.unpack_from(">III", d, 0)
        self.tables = {}
        pos = 12 + n * 12
        for i in range(n):
            size, h, _ = struct.unpack_from(">III", d, 12 + i * 12)
            a = (pos + 15) & ~15
            self.tables[h] = Table(d[a:a + size])
            pos = a + size

    def songs(self):
        """-> {song_id: (title, artist)}"""
        artists = {}
        if ARTISTS in self.tables:
            for r in self.tables[ARTISTS]:          # [artist id, name, ...]
                artists[r[0]] = r[1]
        out = {}
        for r in self.tables[SONGS]:                # [song id, title, artist id, ?, year, ...]
            out[r[0]] = (r[1], artists.get(r[2], ""))
        return out


if __name__ == "__main__":
    db = WiiUDB(sys.argv[1])
    if len(sys.argv) > 2 and sys.argv[2] == "songs":
        for sid, (t, a) in sorted(db.songs().items()):
            print(sid, t, "-", a)
    else:
        for h, t in db.tables.items():
            print("%08x rows=%d rowsize=%d types=%s" % (h, t.rows, t.rowsize, t.types))
