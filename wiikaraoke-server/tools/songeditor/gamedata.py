"""
Reads fixed game constants (the UJK0 section-table XOR key and the real-song
Blowfish key) directly out of a game dump's code/app.rpx, by virtual address —
same approach as tools/make_stub_db.py's read_xor_table, generalized to any
address/size so this package never needs a pre-decompressed app.elf.

Addresses are per the EU build (trunk_eu_3_0); see project_wii_karaoke.md.
"""
import struct
import zlib

XOR_TABLE_VA = 0x1000D7E0
XOR_TABLE_SIZE = 64

BLOWFISH_KEY_VA = 0x100FD660
BLOWFISH_KEY_SIZE = 32

SHF_RPL_ZLIB = 0x08000000
SHT_NOBITS = 8


def read_va(rpx_path, va, size):
    d = open(rpx_path, "rb").read()
    shoff = struct.unpack(">I", d[0x20:0x24])[0]
    shnum = struct.unpack(">H", d[0x30:0x32])[0]
    for i in range(shnum):
        s = struct.unpack(">10I", d[shoff + i * 40:shoff + i * 40 + 40])
        _name, typ, flags, addr, off, secsize = s[:6]
        if typ == SHT_NOBITS or not addr:
            continue
        raw = d[off:off + secsize]
        if flags & SHF_RPL_ZLIB:
            raw = zlib.decompress(raw[4:])
        if addr <= va and va + size <= addr + len(raw):
            return raw[va - addr:va - addr + size]
    raise LookupError("address %#x not found in any loaded section of %s" % (va, rpx_path))


def xor_table(rpx_path):
    return read_va(rpx_path, XOR_TABLE_VA, XOR_TABLE_SIZE)


def blowfish_key(rpx_path):
    return read_va(rpx_path, BLOWFISH_KEY_VA, BLOWFISH_KEY_SIZE)
