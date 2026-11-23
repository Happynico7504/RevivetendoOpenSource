"""RMC framing + dispatch."""
import logging
import struct

from .streams import StreamIn, StreamOut

log = logging.getLogger("rmc")

ERR = {
    "Core::Unknown": 0x00010001, "Core::NotImplemented": 0x00010002, "Core::AccessDenied": 0x00010006,
    "Core::InvalidArgument": 0x0001000A,
    "RendezVous::InvalidUsername": 0x00030064, "RendezVous::InvalidPassword": 0x00030065,
    "RendezVous::NotAuthenticated": 0x00030002,
    "Ranking::InvalidArgument": 0x00670002, "Ranking::NotFound": 0x00670005,
    "Ranking::InvalidDataSize": 0x00670007,
}
SUCCESS = 0x00010001          # qresult "success" as sent by real servers


class RMCError(Exception):
    def __init__(self, name):
        super().__init__(name)
        self.code = ERR[name] | 0x80000000


def parse_request(data):
    s = StreamIn(data)
    size = s.u32()
    proto = s.u8()
    if not proto & 0x80:
        raise ValueError("not an RMC request")
    proto &= 0x7F
    if proto == 0x7F:
        proto = s.u16()
    call_id = s.u32()
    method = s.u32()
    return proto, call_id, method, StreamIn(s.rest())


def response(proto, call_id, method, body):
    hdr = StreamOut()
    if proto < 0x7F:
        hdr.u8(proto)
    else:
        hdr.u8(0x7F).u16(proto)
    hdr.u8(1).u32(call_id).u32(method | 0x8000).write(body)
    b = hdr.bytes()
    return struct.pack("<I", len(b)) + b


def error(proto, call_id, code):
    hdr = StreamOut()
    if proto < 0x7F:
        hdr.u8(proto)
    else:
        hdr.u8(0x7F).u16(proto)
    hdr.u8(0).u32(code).u32(call_id)
    b = hdr.bytes()
    return struct.pack("<I", len(b)) + b


class Dispatcher:
    """protocols: {proto_id: {method_id: handler(conn, StreamIn, StreamOut)}}"""

    def __init__(self, name, protocols, names=None):
        self.name, self.protocols, self.names = name, protocols, names or {}

    def __call__(self, conn, data):
        try:
            proto, call_id, method, args = parse_request(data)
        except Exception as e:
            log.warning("[%s] bad RMC from %s: %s", self.name, conn.addr, e)
            return
        label = "%s.%s" % (self.names.get(proto, proto), method)
        fn = self.protocols.get(proto, {}).get(method)
        if fn is None:
            log.info("[%s] pid=%s %s -> NotImplemented", self.name, conn.pid, label)
            conn.send(error(proto, call_id, ERR["Core::NotImplemented"] | 0x80000000))
            return
        out = StreamOut()
        try:
            fn(conn, args, out)
            conn.send(response(proto, call_id, method, out.bytes()))
            log.info("[%s] pid=%s %s ok (%d bytes)", self.name, conn.pid, label, len(out.buf))
        except RMCError as e:
            log.info("[%s] pid=%s %s -> %s", self.name, conn.pid, label, e)
            conn.send(error(proto, call_id, e.code))
        except Exception:
            log.exception("[%s] pid=%s %s crashed", self.name, conn.pid, label)
            conn.send(error(proto, call_id, ERR["Core::Unknown"] | 0x80000000))
