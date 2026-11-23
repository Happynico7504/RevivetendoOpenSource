#!/usr/bin/env python3
"""
Talks to a server the way app.rpx does and parses replies with Python ports
of the game's own RecvData parsers. Use it to test a server without a console.

  python3 tools/game_client.py http://127.0.0.1:8383 BA0001
  python3 tools/game_client.py http://127.0.0.1:8383 SD0001 sid=0012345 rlog=0
  python3 tools/game_client.py http://127.0.0.1:8383 all
"""
import base64
import struct
import sys
import urllib.parse
import urllib.request

PHP = "control_Orchestra300_EU.php"


def game_quote(s):
    # sub_02113260: "%%%02X" for anything that is not alnum or -._
    return "".join(c if (c.isalnum() and c.isascii()) or c in "-._" else "%%%02X" % ord(c) for c in s)


def enc(v):  # flags 8|16 : base64 then URL-encode
    return game_quote(base64.b64encode(v.encode()).decode())


def build_url(base, typ, extra, common=True):
    parts = [("type", typ)] + list(extra)
    if common:  # sub_02111e58
        parts += [("cid", "110"), ("lid", "1"), ("atk", "TESTTOKEN"), ("ver", "300"), ("gcd", "0")]
    q = "&".join("%s=%s" % (k, enc(v)) for k, v in parts)
    return "%s/%s?%s" % (base.rstrip("/"), PHP, q)


def post_log(base, typ, fields):
    body = "&".join("%s=%s" % (k, enc(v)) for k, v in [("type", typ)] + fields +
                    [("cid", "110"), ("lid", "1"), ("atk", "TESTTOKEN"), ("ver", "300"), ("gcd", "0")])
    return urllib.request.urlopen(urllib.request.Request(
        "%s/%s" % (base.rstrip("/"), PHP), data=body.encode())).read()


class Reader:
    """Port of sub_02018b74.. (big endian) + sub_0213dde0 line reader."""
    def __init__(self, data):
        self.d, self.p = data, 0

    def u8(self):
        v = self.d[self.p]; self.p += 1; return v

    def u16(self):
        v = struct.unpack_from(">H", self.d, self.p)[0]; self.p += 2; return v

    def u32(self):
        v = struct.unpack_from(">I", self.d, self.p)[0]; self.p += 4; return v

    def line(self, maxlen):
        e = self.d.index(b"\r\n", self.p)
        raw = self.d[self.p:e].decode("ascii"); self.p = e + 2
        raw = urllib.parse.unquote_plus(raw)            # '+' -> ' ' like sub_02113a28
        val = base64.b64decode(raw)
        if len(val) > maxlen:
            raise ValueError("line exceeds game buffer %d" % maxlen)
        return val.decode("utf-8", "replace")

    def header(self):  # sub_0213df60
        cat = self.u8(); b = self.u8()
        if cat == 0:
            return {"ok": b == 1, "result": b}
        msg = self.line(512) if b > 0 else ""
        return {"ok": False, "category": cat, "message": msg}


PARSERS = {
    "SA0001": lambda r: {"token_expiry": r.line(16), "server_now": r.line(16)},
    "TE0001": lambda r: {"purchases": r.u32()},
    "BA0001": lambda r: {"status": r.u8(), "expiry": r.line(16), "server_now": r.line(16), "permanent": r.u8()},
    "SD0001": lambda r: {"size": r.u32(), "url": r.line(512)},
    "WO0001": lambda r: {"size": r.u32(), "url": r.line(512)},
    "MD0001": lambda r: {"size": r.u32(), "url": r.line(512)},
    "GP0001": lambda r: {"size": r.u32(), "url": r.line(512)},
    "GF0001": lambda r: {"size": r.u32(), "url": r.line(512)},
    "GD0001": lambda r: {"id": r.line(33), "size": r.u32(), "url": r.line(512)},
    "GM0001": lambda r: {"id": r.line(33), "size": r.u32(), "url": r.line(512)},
    "FE0001": lambda r: {"id": r.line(33), "size": r.u32(), "url": r.line(513)},
    "MA0001": lambda r: {"id": r.line(33), "size": r.u32(), "url": r.line(513)},
    "GL0001": lambda r: (lambda n: {"count": n, "ids": [r.line(32) for _ in range(n)]})(r.u16()),
    "NI0002": lambda r: (lambda has: {"has": has, **({"size": r.u32(), "url": r.line(512)} if has == 1 else {})})(r.u8()),
}


def call(base, typ, extra=()):
    if typ.startswith("LG"):
        data = post_log(base, typ, list(extra))
    else:
        data = urllib.request.urlopen(build_url(base, typ, extra)).read()
    r = Reader(data)
    h = r.header()
    out = {"type": typ, "header": h}
    if h["ok"] and typ in PARSERS:
        out["body"] = PARSERS[typ](r)
    out["trailing_bytes"] = len(data) - r.p
    return out


if __name__ == "__main__":
    base, typ = sys.argv[1], sys.argv[2]
    extra = [tuple(a.split("=", 1)) for a in sys.argv[3:]]
    if typ == "all":
        tests = [("SA0001", []), ("TE0001", []), ("BA0001", []), ("NI0002", []),
                 ("GD0001", []), ("GM0001", []), ("GL0001", [("mdid", "1")]),
                 ("GP0001", [("pdid", "p001")]), ("GF0001", [("dbid", "1")]),
                 ("SD0001", [("sid", "0012345"), ("rlog", "0")]), ("WO0001", [("sid", "12345")]),
                 ("MD0001", [("mid", "1"), ("mno", "01")]), ("FE0001", []),
                 ("MA0001", [("cid", "00"), ("lid", "1")]),
                 ("LG0002", [("styp", "1"), ("sid", "12345")])]
        for t, e in tests:
            print(call(base, t, e))
    else:
        print(call(base, typ, extra))
