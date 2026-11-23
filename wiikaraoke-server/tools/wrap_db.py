#!/usr/bin/env python3
"""
Manually wrap a plain wiiudb.dat/song_begin_db.dat-format file into the real
GD0001/GF0001 wire format: XOR with the 1024-byte cyclic table pulled straight
from the game's own app.rpx, then append the little-endian CRC32 trailer
make_stub_db.py's stub() already implements - see that file's own docstring
for the full format writeup (sub_021E6324 XOR, sub_0210E2A0 check).

Use this to try variations by hand (edit a field, rewrap, compare) without
re-running the full add_custom_song.py/make_stub_db.py pipeline each time.

  python3 tools/wrap_db.py <plain.bin> <out.bin> main
  python3 tools/wrap_db.py <plain.bin> <out.bin> firstline
  python3 tools/wrap_db.py <plain.bin> <out.bin> main --rpx /path/to/app.rpx
  python3 tools/wrap_db.py <wrapped.bin> <out.bin> main --unwrap   # de-xor + verify trailer, don't re-wrap
"""
import argparse
import struct
import sys
import zlib

from make_stub_db import (
    read_xor_table, stub, xor_head,
    XOR_TABLE_MAIN, XOR_TABLE_FIRST_LINE, XOR_SIZE, TRAILER_SIZE, SHORT_CHECK_PADDING,
)

DEFAULT_RPX = "/home/nico/wiikaraokeu/code/app.rpx"


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("infile")
    ap.add_argument("outfile")
    ap.add_argument("kind", choices=["main", "firstline"], help="which XOR table/padding rule to use")
    ap.add_argument("--rpx", default=DEFAULT_RPX, help="path to app.rpx (default: %(default)s)")
    ap.add_argument("--unwrap", action="store_true", help="de-XOR + verify the trailer instead of wrapping")
    ap.add_argument("--no-pad", action="store_true", help="skip the 0x19000 firstline padding even for kind=firstline")
    args = ap.parse_args()

    va = XOR_TABLE_MAIN if args.kind == "main" else XOR_TABLE_FIRST_LINE
    table = read_xor_table(args.rpx, va)
    data = open(args.infile, "rb").read()

    if args.unwrap:
        dexored = xor_head(data, table)
        pad = 0 if args.no_pad or args.kind == "main" else SHORT_CHECK_PADDING
        if pad:
            dexored = dexored[:-pad] if len(dexored) > pad else dexored
        body = dexored[:-TRAILER_SIZE]
        trailer = dexored[-TRAILER_SIZE:]
        stored_crc = struct.unpack("<I", trailer[:4])[0]
        real_crc = zlib.crc32(body) & 0xFFFFFFFF
        zero_pad_ok = all(b == 0 for b in trailer[4:])
        print("body len:", len(body))
        print("stored crc:", hex(stored_crc), "computed crc:", hex(real_crc), "match:", stored_crc == real_crc)
        print("trailer zero padding ok:", zero_pad_ok)
        open(args.outfile, "wb").write(body)
        print("wrote de-xored body to", args.outfile)
    else:
        pad = 0 if args.no_pad or args.kind == "main" else SHORT_CHECK_PADDING
        wrapped = stub(data, table) + b"\0" * pad
        open(args.outfile, "wb").write(wrapped)
        print("wrote", args.outfile, len(wrapped), "bytes (body", len(data), "+ trailer", TRAILER_SIZE, "+ padding", pad, ")")


if __name__ == "__main__":
    sys.exit(main())
