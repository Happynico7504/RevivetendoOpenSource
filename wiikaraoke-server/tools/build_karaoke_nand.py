#!/usr/bin/env python3
"""
Build the complete, full-size data_base01-04.bin NAND files (served at
/content/karaoke_nand/) plus their version.txt marker, from the catalog's
wrapped GD0001 form (content/db/main.bin, the same file server.py serves
over HTTP - XORed with the game's own 1024-byte cyclic table plus a
trailing CRC32, see make_stub_db.py/wrap_db.py).

CONFIRMED BUG, fixed here: that wrapped form is exactly right for the
GD0001 HTTP download, which the game itself de-XORs and verifies on
arrival - but the NAND load path (data_base01-04.bin, read directly from
save data, never transmitted) needs the PLAIN, already-unwrapped body.
Feeding the wrapped file straight into the NAND split put still-XORed
bytes exactly where the game expects plain table data, which it tries to
parse directly without ever de-XORing. This script now unwraps first.

The combined on-NAND buffer is one contiguous 0x3200200-byte region split
across the 4 files at fixed offsets/sizes; everything past the real content
is zero, matching the real console's own layout. See feedback_wiiudb_header
in the add_custom_song.py docstring for the 0x200-byte header fields this
reuses unchanged (format version etc.) aside from length/checksum/id.

SECOND CONFIRMED FIX: the header's first 32 bytes are the same "id" string
GD0001's HTTP response reports (wahp/handlers.py: `.line(item["id"], 32)`,
an MD5 hex digest of content/db/main.bin) - the game compares this stored
id against what a live GD0001 request returns to decide whether it
already has the current catalog (wahp/handlers.py:141's "the game skips
the download when this id equals the one in its save"). Every build
before this one just copied the header template's id field verbatim
(the disc's own original, never-updated "2014040400_0" placeholder),
which can never match the server's real MD5 id - so the game always
concluded an update was needed and always had to go through its own
online-update flow (the one patch 1/2 exist to work around in the first
place, and the one actually confirmed broken for a catalog installed
this way). Writing the real, current id here instead lets the game's own
unpatched freshness check succeed naturally and skip that flow entirely -
no game-code patch needed at all.

  python3 tools/build_karaoke_nand.py <wrapped_main.bin> <header_template.bin> content/karaoke_nand/ [--rpx PATH]
"""
import argparse
import hashlib
import os
import struct
import zlib

from make_stub_db import read_xor_table, xor_head, XOR_TABLE_MAIN, TRAILER_SIZE

TOTAL = 0x3200200
SPLITS = [(0, 0x1000000), (0x1000000, 0x1000000), (0x2000000, 0x1000000), (0x3000000, 0x200200)]
DEFAULT_RPX = "/home/nico/wiikaraokeu/code/app.rpx"


def unwrap_main(wrapped, rpx_path):
    table = read_xor_table(rpx_path, XOR_TABLE_MAIN)
    dexored = xor_head(wrapped, table)
    body = dexored[:-TRAILER_SIZE]
    trailer = dexored[-TRAILER_SIZE:]
    stored_crc = struct.unpack("<I", trailer[:4])[0]
    real_crc = zlib.crc32(body) & 0xFFFFFFFF
    if stored_crc != real_crc:
        raise SystemExit(f"unwrap failed: stored crc {stored_crc:#x} != computed {real_crc:#x} "
                          "(wrong --rpx, or input wasn't actually wrapped main.bin-format)")
    return body


def catalog_db_id(catalog_path):
    """The same id GD0001's HTTP response reports for this exact wrapped
    file - see wahp/content.py's md5()."""
    return hashlib.md5(open(catalog_path, "rb").read()).hexdigest().encode("ascii")


def deployed_db_id(out_dir):
    """The id baked into the first file's header on a previous build, or
    None if it's missing/short (no build yet). Used to detect whether
    content/db/main.bin has moved on since the NAND files were last built,
    so a server restart can rebuild them automatically instead of serving
    a catalog whose on-NAND form is stale."""
    path = os.path.join(out_dir, "data_base01.bin")
    try:
        hdr = open(path, "rb").read(32)
    except OSError:
        return None
    if len(hdr) < 32:
        return None
    return hdr.rstrip(b"\0")


def needs_rebuild(catalog_path, out_dir):
    if not os.path.exists(os.path.join(out_dir, "version.txt")):
        return True
    return deployed_db_id(out_dir) != catalog_db_id(catalog_path)


def build(catalog_path, header_template_path, out_dir, rpx_path=DEFAULT_RPX):
    wrapped = open(catalog_path, "rb").read()
    content = unwrap_main(wrapped, rpx_path)
    old_hdr = open(header_template_path, "rb").read()[:0x200]
    # CONFIRMED BUG, fixed here: this used to read the WHOLE template file
    # with no truncation. Passing a full data_base01.bin (or anything
    # bigger than 0x200 bytes) as the template made `buf[0:0x200] =
    # new_hdr` below a length-mismatched slice assignment, which Python
    # resizes rather than rejects - silently ballooning `buf` far past
    # TOTAL. The CRC then got computed over that wrong, oversized buffer
    # while only the first TOTAL bytes of it were ever written to disk, so
    # the stored CRC never matched the persisted files - exactly the
    # "corrupted catalog" symptom, with no error anywhere in this script.
    assert len(old_hdr) == 0x200, "header template shorter than 0x200 bytes"

    # Computed over the WRAPPED file (catalog_path), matching catalog_db_id()
    # above - that's what the server actually serves/hashes, not the
    # unwrapped body. Stored in the output header so a later run can tell
    # via deployed_db_id()/needs_rebuild() whether a rebuild is needed.
    db_id = hashlib.md5(wrapped).hexdigest().encode("ascii")

    new_hdr = bytearray(old_hdr)
    new_hdr[0:32] = b"\0" * 32
    new_hdr[0:len(db_id)] = db_id
    struct.pack_into(">I", new_hdr, 0x20, len(content))
    struct.pack_into(">I", new_hdr, 0x24, len(content))
    struct.pack_into(">I", new_hdr, 0x28, 0)

    buf = bytearray(TOTAL)
    buf[0:0x200] = new_hdr
    buf[0x200:0x200 + len(content)] = content
    crc = zlib.crc32(bytes(buf)) & 0xFFFFFFFF
    struct.pack_into(">I", buf, 0x28, crc)

    os.makedirs(out_dir, exist_ok=True)
    h = hashlib.sha256()
    written = []
    for i, (off, size) in enumerate(SPLITS, start=1):
        chunk = bytes(buf[off:off + size])
        assert len(chunk) == size
        path = os.path.join(out_dir, f"data_base{i:02d}.bin")
        open(path, "wb").write(chunk)
        h.update(chunk)
        written.append((path, len(chunk)))

    version = h.hexdigest()[:16]
    version_path = os.path.join(out_dir, "version.txt")
    with open(version_path, "w") as f:
        f.write(version + "\n")
    written.append((version_path, None))
    return version, written


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("catalog_path", help="wrapped main.bin-format catalog (e.g. content/db/main.bin)")
    ap.add_argument("header_template_path")
    ap.add_argument("out_dir")
    ap.add_argument("--rpx", default=DEFAULT_RPX, help="path to app.rpx (default: %(default)s)")
    args = ap.parse_args()

    version, written = build(args.catalog_path, args.header_template_path, args.out_dir, args.rpx)
    for path, size in written:
        print("wrote", path, *(() if size is None else (size, "bytes")))
    print("version", version)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
