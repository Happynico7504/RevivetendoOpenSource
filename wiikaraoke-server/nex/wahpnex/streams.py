"""NEX byte streams (little endian) for NEX 3.2.1 - no structure version headers."""
import struct
from datetime import datetime, timezone


class StreamOut:
    def __init__(self):
        self.buf = bytearray()

    def write(self, b): self.buf += b; return self
    def u8(self, v): self.buf += struct.pack("<B", v & 0xFF); return self
    def u16(self, v): self.buf += struct.pack("<H", v & 0xFFFF); return self
    def u32(self, v): self.buf += struct.pack("<I", v & 0xFFFFFFFF); return self
    def u64(self, v): self.buf += struct.pack("<Q", v & 0xFFFFFFFFFFFFFFFF); return self
    def s32(self, v): self.buf += struct.pack("<i", v); return self
    def f64(self, v): self.buf += struct.pack("<d", v); return self
    def bool(self, v): return self.u8(1 if v else 0)
    pid = u32          # Wii U: PIDs are 32-bit
    result = u32

    def string(self, s):
        if s is None:
            return self.u16(0)
        data = s.encode("utf-8") + b"\0"
        return self.u16(len(data)).write(data)

    def buffer(self, b): return self.u32(len(b)).write(b)
    def qbuffer(self, b): return self.u16(len(b)).write(b)
    def qvector_u8(self, b): return self.u32(len(b)).write(bytes(b))
    def datetime(self, v): return self.u64(v)

    def list(self, items, fn):
        self.u32(len(items))
        for it in items:
            fn(self, it)
        return self

    def bytes(self): return bytes(self.buf)


class StreamIn:
    def __init__(self, data):
        self.d = data
        self.p = 0

    def read(self, n):
        if self.p + n > len(self.d):
            raise ValueError("stream underflow")
        v = self.d[self.p:self.p + n]; self.p += n; return v

    def u8(self): return self.read(1)[0]
    def u16(self): return struct.unpack("<H", self.read(2))[0]
    def u32(self): return struct.unpack("<I", self.read(4))[0]
    def u64(self): return struct.unpack("<Q", self.read(8))[0]
    def bool(self): return self.u8() != 0
    pid = u32

    def string(self):
        n = self.u16()
        return self.read(n).rstrip(b"\0").decode("utf-8", "replace") if n else ""

    def buffer(self): return self.read(self.u32())
    def qbuffer(self): return self.read(self.u16())
    def qvector_u8(self): return self.read(self.u32())
    def datetime(self): return self.u64()

    def list(self, fn):
        return [fn(self) for _ in range(self.u32())]

    def anydata(self):
        """AnyDataHolder -> (type_name, payload bytes)"""
        name = self.string()
        self.u32()              # length incl. next field
        return name, self.buffer()

    def eof(self): return self.p >= len(self.d)
    def rest(self): return self.d[self.p:]


def nex_datetime(dt=None):
    dt = dt or datetime.now(timezone.utc)
    return (dt.year << 26) | (dt.month << 22) | (dt.day << 17) | (dt.hour << 12) | (dt.minute << 6) | dt.second


def nex_datetime_to_ts(v):
    try:
        return datetime((v >> 26), (v >> 22) & 15, (v >> 17) & 31, (v >> 12) & 31,
                        (v >> 6) & 63, v & 63, tzinfo=timezone.utc).timestamp()
    except ValueError:
        return 0


def station_url(scheme="prudps", **params):
    return "%s:/%s" % (scheme, ";".join("%s=%s" % kv for kv in params.items()))


def parse_station_url(s):
    if ":/" not in s:
        return "", {}
    scheme, rest = s.split(":/", 1)
    params = {}
    for part in rest.split(";"):
        if "=" in part:
            k, v = part.split("=", 1)
            params[k] = v
    return scheme, params
