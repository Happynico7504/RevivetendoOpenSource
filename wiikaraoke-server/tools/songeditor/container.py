"""
UJK0 container — the top-level format for every song/jingle file (streamed
.ujk files served by this project, and real on-disc/bundled .ujk files alike).

Layout: "UJK0" + u32 BE table_offset(=0x20) + u32 BE end + u32 BE crc32 +
16 zero bytes, then a 64-byte section table at offset 0x20, XOR'd with the
game's fixed 64-byte table (gamedata.xor_table), holding up to 8 big-endian
(offset, length) u32 pairs — body data follows at offset 0x60. CRC is
zlib.crc32 seeded with b"XING", computed over the *plain* (un-XORed) table
plus everything after it.

Real on-disc/bundled files additionally wrap the whole thing in Blowfish
ECB (see read_real) — streamed files this project serves do not (checked:
every test-*.ujk built this session is plain, unencrypted UJK0).

Known section slots (by NT index, i.e. NT[slot*2], NT[slot*2+1]):
  0 = audio (TPSA for streamed, 4PMA for real/bundled — see audio.py)
  1 = PNG background image
  2 = JOY-U1 blob (SSZL-compressed; see track.py for what's inside)
  3 = font/glyph bitmap block (SSZL-compressed)
Real files don't necessarily use this same slot order (error.ujk's own
audio was slot 0 at file offset 96, i.e. right after the header) — don't
assume a fixed body layout, always read it from the table.
"""
import struct
import zlib

HEADER_SIZE = 0x60
TABLE_OFFSET = 0x20
TABLE_SIZE = 0x40
CRC_SEED = 0x58494E47  # "XING"


def _xor(data, key):
    return bytes(a ^ b for a, b in zip(data, key * (len(data) // len(key) + 1)))


def read_table(data, xk):
    plain = _xor(data[TABLE_OFFSET:TABLE_OFFSET + TABLE_SIZE], xk)
    return list(struct.unpack(">16I", plain))


def read_section(data, table, slot):
    off, size = table[slot * 2], table[slot * 2 + 1]
    return data[off:off + size]


def read_real(path, key, xk):
    """Decrypt a real on-disc/bundled .ujk (Blowfish ECB) and return the plain bytes."""
    from Crypto.Cipher import Blowfish

    raw = open(path, "rb").read()
    n = len(raw) & ~7
    dec = Blowfish.new(key, Blowfish.MODE_ECB).decrypt(raw[:n]) + raw[n:]
    assert dec[:4] == b"UJK0", "not a UJK0 file after Blowfish decrypt"
    return dec


def build(sections, xk):
    """sections: {slot_index: bytes}. Returns a plain (unencrypted) UJK0 file.

    Body order matters, independent of the table's slot numbering: a
    streamed (TPSA) song whose audio section is placed anywhere but LAST
    in the body freezes the game (confirmed on hardware 2026-11-07, by
    diffing against error.ujk/aerials-test.ujk's own layout, both of
    which carry audio last). Slot 0 (audio) is always written after every
    other section regardless of what order its key appears in `sections`.
    """
    n_slots = max(sections) + 1
    body_slots = [s for s in range(n_slots) if s != 0] + ([0] if 0 in sections else [])
    body = bytearray()
    table = [0] * 16
    pos = HEADER_SIZE
    for slot in body_slots:
        data = sections.get(slot, b"")
        if not data:
            continue
        table[slot * 2] = pos
        table[slot * 2 + 1] = len(data)
        body += data
        pos += len(data)
    end = pos

    table_plain = b"".join(struct.pack(">I", x) for x in table)
    table_encoded = _xor(table_plain, xk)
    crc = zlib.crc32(table_plain + bytes(body), CRC_SEED) & 0xFFFFFFFF
    out = (
        b"UJK0"
        + struct.pack(">III", TABLE_OFFSET, end, crc)
        + b"\0" * 16
        + table_encoded
        + bytes(body)
    )
    return out
