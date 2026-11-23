"""
SSZL compression — see feedback_sszl_encoder_bug.md for the full story of why
this exists: the game's decompressor silently fails (permanently-white stage,
no crash) if you hand it a literal-only "compression" that's larger than the
plain data, which an earlier, naive encoder did. This is a real LZSS encoder.

Format: "SSZL" + 4 zero bytes + u32 LE compressed_size + u32 LE plain_size +
body. Body = repeating {1 flag byte, up to 8 tokens}; flag bits read LSB
first (`fl >>= 1` per token) — bit=1 means the next byte is a literal, bit=0
means the next 2 bytes are a back-reference: `offset = a | ((b & 0xf0) << 4)`,
`length = (b & 0xf) + 3`. 4096-byte ring buffer, write cursor starts at
0xfee (the classic LZSS convention this format happens to share).
"""
import struct


def decompress(data):
    assert data[:4] == b"SSZL"
    _psize, usize = struct.unpack("<II", data[8:16])
    out = bytearray()
    ring = bytearray(4096)
    r = 0xFEE
    p = 16
    n = len(data)
    while len(out) < usize and p < n:
        flags = data[p]
        p += 1
        for _ in range(8):
            if len(out) >= usize:
                break
            if flags & 1:
                c = data[p]
                p += 1
                out.append(c)
                ring[r] = c
                r = (r + 1) & 4095
            else:
                a, b = data[p], data[p + 1]
                p += 2
                o = a | ((b & 0xF0) << 4)
                length = (b & 0xF) + 3
                for k in range(length):
                    c = ring[(o + k) & 4095]
                    out.append(c)
                    ring[r] = c
                    r = (r + 1) & 4095
            flags >>= 1
    return bytes(out)


def compress(data, min_match=3, max_match=18):
    n = len(data)
    win = bytearray(4096)
    r = 0xFEE
    pos = 0
    pending = []

    def win_write(c):
        nonlocal r
        win[r] = c
        r = (r + 1) & 4095

    while pos < n:
        best_len = 0
        best_off = 0
        if pos >= min_match:
            search_start = max(0, pos - 4095)
            max_possible = min(max_match, n - pos)
            for cand in range(pos - 1, search_start - 1, -1):
                length = 0
                while length < max_possible and data[cand + length] == data[pos + length]:
                    length += 1
                if length > best_len:
                    best_len, best_off = length, cand
                if best_len >= max_match:
                    break
        if best_len >= min_match:
            o = (0xFEE + best_off) & 4095
            lo = o & 0xFF
            hi = ((o >> 4) & 0xF0) | (best_len - 3)
            pending.append((False, (lo, hi)))
            for k in range(best_len):
                win_write(data[pos + k])
            pos += best_len
        else:
            c = data[pos]
            pending.append((True, c))
            win_write(c)
            pos += 1

    out_body = bytearray()
    i = 0
    while i < len(pending):
        chunk = pending[i:i + 8]
        flag = 0
        body = bytearray()
        for k, (is_lit, val) in enumerate(chunk):
            if is_lit:
                flag |= 1 << k
                body.append(val)
            else:
                body.append(val[0])
                body.append(val[1])
        out_body.append(flag)
        out_body += body
        i += 8

    header = b"SSZL" + b"\0\0\0\0" + struct.pack("<II", len(out_body), n)
    return bytes(header) + bytes(out_body)
