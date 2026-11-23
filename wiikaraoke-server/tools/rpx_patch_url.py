#!/usr/bin/env python3
"""
Repoint Wii Karaoke U (EU) at your own server by rewriting the base URL
string in app.rpx, then rebuild the RPX (recompressed .rodata, fixed CRC).

  python3 tools/rpx_patch_url.py app.rpx app_patched.rpx http://192.168.1.10:8383/

The game builds   <base URL> + "control_Orchestra300_EU.php"   (sub_02112d68),
the base lives at .rodata 0x1001D0E4 and has room for 47 characters.
Use http:// so no SSL patch is needed (curl only uses the NSSL context for https).
"""
import struct
import sys
import zlib

BASE_VA = 0x1001D0E4
SLOT = 0x1001D114 - BASE_VA          # 48 bytes incl. terminator
ORIG = b"https://ssl.wahp.wah.wup.app.nintendo.net/"
SHF_RPL_ZLIB = 0x08000000
SHT_RPL_CRCS = 0x80000003


def main(src, dst, url):
    if not url.endswith("/"):
        url += "/"
    new = url.encode("ascii")
    if len(new) > SLOT - 1:
        sys.exit("URL too long: %d chars, max %d" % (len(new), SLOT - 1))

    d = bytearray(open(src, "rb").read())
    shoff = struct.unpack(">I", d[0x20:0x24])[0]
    shnum = struct.unpack(">H", d[0x30:0x32])[0]
    secs = [list(struct.unpack(">10I", d[shoff + i * 40:shoff + i * 40 + 40])) for i in range(shnum)]
    raws = [bytes(d[s[4]:s[4] + s[5]]) if (s[1] != 8 and s[5]) else b"" for s in secs]

    # find the section that contains BASE_VA
    cands = [i for i, s in enumerate(secs) if s[1] == 1 and 0 < s[3] <= BASE_VA]
    idx = max(cands, key=lambda i: secs[i][3])
    s = secs[idx]
    data = bytearray(zlib.decompress(raws[idx][4:]) if s[2] & SHF_RPL_ZLIB else raws[idx])
    off = BASE_VA - s[3]
    if not data[off:off + len(ORIG)] == ORIG:
        cur = bytes(data[off:off + SLOT]).split(b"\0")[0]
        print("note: URL slot currently holds %r (already patched?)" % cur)
    data[off:off + SLOT] = new + b"\0" * (SLOT - len(new))
    crc = zlib.crc32(data)
    if s[2] & SHF_RPL_ZLIB:
        raws[idx] = struct.pack(">I", len(data)) + zlib.compress(bytes(data), 6)
    else:
        raws[idx] = bytes(data)

    # update CRC table
    ci = next(i for i, t in enumerate(secs) if t[1] == SHT_RPL_CRCS)
    tbl = bytearray(raws[ci])
    struct.pack_into(">I", tbl, idx * 4, crc)
    raws[ci] = bytes(tbl)

    # re-layout: keep original file order, 64-byte alignment
    order = sorted((i for i in range(shnum) if raws[i]), key=lambda i: secs[i][4])
    out = bytearray(d[:shoff + shnum * 40])
    pos = len(out)
    for i in order:
        pos = (pos + 63) & ~63
        out += b"\0" * (pos - len(out))
        secs[i][4] = pos
        secs[i][5] = len(raws[i])
        out += raws[i]
        pos = len(out)
    for i, t in enumerate(secs):
        struct.pack_into(">10I", out, shoff + i * 40, *t)
    open(dst, "wb").write(out)

    # verify
    chk = bytearray(out)
    s2 = [struct.unpack(">10I", chk[shoff + i * 40:shoff + i * 40 + 40]) for i in range(shnum)]
    t = s2[idx]
    v = zlib.decompress(bytes(chk[t[4] + 4:t[4] + t[5]])) if t[2] & SHF_RPL_ZLIB else bytes(chk[t[4]:t[4] + t[5]])
    assert v[off:off + len(new)] == new and zlib.crc32(v) == crc
    print("patched base URL -> %s  (requests go to %scontrol_Orchestra300_EU.php)" % (url, url))
    print("wrote", dst, len(out), "bytes")


if __name__ == "__main__":
    if len(sys.argv) != 4:
        sys.exit(__doc__)
    main(*sys.argv[1:])
