"""
PRUDP v1 server (Wii U NEX) over asyncio UDP.

Packet = EA D0 | header(12) | signature(16) | options | payload
header = ver(1)=1, optlen, payload_size(2), src, dst, type|flags<<4 (2), session, substream, seq(2)
Signature = HMAC-MD5(key=MD5(access_key), header[4:] + session_key + u32(sum(access_key))
                     + connection_signature + options + payload)
"""
import asyncio
import hashlib
import hmac
import logging
import os
import random
import struct
import time

from .crypto import RC4

log = logging.getLogger("prudp")

SYN, CONNECT, DATA, DISCONNECT, PING = 0, 1, 2, 3, 4
F_ACK, F_RELIABLE, F_NEED_ACK, F_HAS_SIZE, F_MULTI_ACK = 1, 2, 4, 8, 0x200
TYPE_NAMES = {0: "SYN", 1: "CONNECT", 2: "DATA", 3: "DISCONNECT", 4: "PING"}

CONN_SIG_KEY = os.urandom(16)
DEFAULT_KEY = b"CD&ML"

# This server is reachable from the public internet: bound what one sender can make us hold.
MAX_PENDING_SYNS = 4096
MAX_CONNECTIONS = 4096
MAX_RELIABLE_WINDOW = 256
MAX_MESSAGE_SIZE = 64 * 1024


class Packet:
    __slots__ = ("type", "flags", "src", "dst", "session", "substream", "seq", "signature",
                 "minor", "functions", "conn_sig", "unrel_seq", "max_substream", "fragment",
                 "payload", "options_raw", "header_raw")

    def __init__(self, type=0, flags=0):
        self.type, self.flags = type, flags
        self.src = self.dst = 0
        self.session = self.substream = self.seq = 0
        self.signature = b"\0" * 16
        self.minor = self.functions = 0
        self.conn_sig = b"\0" * 16
        self.unrel_seq = 0
        self.max_substream = 0
        self.fragment = 0
        self.payload = b""
        self.options_raw = None
        self.header_raw = None

    def options(self):
        o = b""
        if self.type in (SYN, CONNECT):
            o += struct.pack("<BBI", 0, 4, self.minor | (self.functions << 8))
            o += struct.pack("<BB", 1, 16) + self.conn_sig
            if self.type == CONNECT:
                o += struct.pack("<BBH", 3, 2, self.unrel_seq)
            o += struct.pack("<BBB", 4, 1, self.max_substream)
        elif self.type == DATA:
            o += struct.pack("<BBB", 2, 1, self.fragment)
        return o

    def header(self, optlen):
        return struct.pack("<BBHBBHBBH", 1, optlen, len(self.payload), self.src, self.dst,
                           self.type | (self.flags << 4), self.session, self.substream, self.seq)

    def __repr__(self):
        fl = [n for f, n in ((F_ACK, "ACK"), (F_RELIABLE, "REL"), (F_NEED_ACK, "NEED_ACK"),
                             (F_HAS_SIZE, "SIZE"), (F_MULTI_ACK, "MULTI")) if self.flags & f]
        return "<%s %s seq=%d frag=%d len=%d>" % (TYPE_NAMES.get(self.type, self.type), "|".join(fl),
                                                  self.seq, self.fragment, len(self.payload))


def decode(data):
    out, p = [], 0
    while p < len(data):
        if data[p:p + 2] != b"\xEA\xD0":
            raise ValueError("bad magic")
        hdr = data[p + 2:p + 14]
        ver, optlen, size, src, dst, tf, sess, sub, seq = struct.unpack("<BBHBBHBBH", hdr)
        if ver != 1:
            raise ValueError("not PRUDP v1")
        pk = Packet(tf & 0xF, tf >> 4)
        pk.src, pk.dst, pk.session, pk.substream, pk.seq = src, dst, sess, sub, seq
        pk.signature = data[p + 14:p + 30]
        opts = data[p + 30:p + 30 + optlen]
        q = 0
        while q < len(opts):
            oid, olen = opts[q], opts[q + 1]
            v = opts[q + 2:q + 2 + olen]
            if oid == 0:
                x = struct.unpack("<I", v)[0]; pk.minor, pk.functions = x & 0xFF, x >> 8
            elif oid == 1: pk.conn_sig = v
            elif oid == 2: pk.fragment = v[0]
            elif oid == 3: pk.unrel_seq = struct.unpack("<H", v)[0]
            elif oid == 4: pk.max_substream = v[0]
            q += 2 + olen
        pk.options_raw, pk.header_raw = opts, hdr
        pk.payload = data[p + 30 + optlen:p + 30 + optlen + size]
        out.append(pk)
        p += 30 + optlen + size
    return out


class Codec:
    def __init__(self, access_key: str):
        self.ak = access_key.encode()
        self.hkey = hashlib.md5(self.ak).digest()
        self.aksum = struct.pack("<I", sum(self.ak))

    def sign(self, pk, session_key, conn_sig, options=None, header=None):
        options = pk.options() if options is None else options
        header = pk.header(len(options)) if header is None else header
        m = hmac.new(self.hkey, digestmod=hashlib.md5)
        for part in (header[4:], session_key, self.aksum, conn_sig, options, pk.payload):
            m.update(part)
        return m.digest()

    def encode(self, pk, session_key, conn_sig):
        opts = pk.options()
        hdr = pk.header(len(opts))
        pk.signature = self.sign(pk, session_key, conn_sig, opts, hdr)
        return b"\xEA\xD0" + hdr + pk.signature + opts + pk.payload

    def verify(self, pk, session_key, conn_sig):
        return hmac.compare_digest(self.sign(pk, session_key, conn_sig, pk.options_raw, pk.header_raw), pk.signature)

    @staticmethod
    def connection_signature(addr):
        ip = bytes(int(x) for x in addr[0].split("."))
        return hmac.digest(CONN_SIG_KEY, ip + struct.pack(">H", addr[1]), "md5")


class Connection:
    def __init__(self, server, addr, syn_pk, connect_pk):
        self.server, self.addr = server, addr
        self.client_port, self.server_port = connect_pk.src, connect_pk.dst
        self.minor, self.functions = connect_pk.minor, connect_pk.functions
        self.max_substream = connect_pk.max_substream
        self.client_session = connect_pk.session
        self.session = random.randint(1, 255)
        self.client_conn_sig = connect_pk.conn_sig          # used when *we* sign
        self.server_conn_sig = Codec.connection_signature(addr)   # used by client when signing
        self.session_key = b""
        self.pid = None
        self.cid = None
        self.rc4_in = RC4(DEFAULT_KEY)
        self.rc4_out = RC4(DEFAULT_KEY)
        self.expect = 2                  # client's CONNECT used reliable seq 1
        self.pending = {}                # out-of-order reliable packets
        self.frag_buf = b""
        self.out_seq = 1
        self.unacked = {}                # seq -> [raw bytes, last_send, tries]
        self.last_seen = time.time()
        self.closed = False
        self.ctx = {}                    # per-connection state for protocol handlers
        self.connection_id = server.next_cid()

    def set_session_key(self, key):
        self.session_key = key
        self.rc4_in = RC4(key)
        self.rc4_out = RC4(key)

    # --- sending -----------------------------------------------------------
    def _base(self, type, flags):
        pk = Packet(type, flags)
        pk.src, pk.dst = self.server_port, self.client_port
        pk.session = self.session
        return pk

    def raw_send(self, pk):
        data = self.server.codec.encode(pk, self.session_key, self.client_conn_sig)
        self.server.transport.sendto(data, self.addr)
        return data

    def send_ack(self, pk):
        ack = self._base(pk.type, F_ACK)
        ack.seq, ack.fragment, ack.substream = pk.seq, pk.fragment, pk.substream
        for _ in range(3 if pk.type == DISCONNECT else 1):
            self.raw_send(ack)

    def send(self, data):
        """Send an RMC message reliably (fragmented, RC4 encrypted in order)."""
        size = self.server.fragment_size
        chunks = [data[i:i + size] for i in range(0, len(data), size)] or [b""]
        for n, chunk in enumerate(chunks):
            pk = self._base(DATA, F_RELIABLE | F_NEED_ACK | F_HAS_SIZE)
            pk.fragment = 0 if n == len(chunks) - 1 else n + 1
            pk.seq = self.out_seq
            self.out_seq = (self.out_seq + 1) & 0xFFFF
            pk.payload = self.rc4_out.crypt(chunk)
            raw = self.raw_send(pk)
            self.unacked[pk.seq] = [raw, time.time(), 0]

    def disconnect(self):
        if not self.closed:
            pk = self._base(DISCONNECT, 0)
            for _ in range(3):
                self.raw_send(pk)
            self.close()

    def close(self):
        self.closed = True
        self.server.connections.pop((self.addr, self.client_port), None)
        self.server.on_close(self)

    # --- receiving ---------------------------------------------------------
    def handle(self, pk):
        self.last_seen = time.time()
        if pk.flags & F_ACK or pk.flags & F_MULTI_ACK:
            if pk.flags & F_MULTI_ACK:
                self._multi_ack(pk)
            elif pk.type == DATA:
                self.unacked.pop(pk.seq, None)
            return
        if pk.flags & F_NEED_ACK:
            self.send_ack(pk)
        if pk.flags & F_RELIABLE:
            if ((pk.seq - self.expect) & 0xFFFF) >= 0x8000:
                return                               # already processed (duplicate)
            if ((pk.seq - self.expect) & 0xFFFF) >= MAX_RELIABLE_WINDOW:
                return
            self.pending[pk.seq] = pk
            while self.expect in self.pending:
                p = self.pending.pop(self.expect)
                self.expect = (self.expect + 1) & 0xFFFF
                self._in_order(p)
        elif pk.type == DISCONNECT:
            self.close()

    def _in_order(self, pk):
        if pk.type == DATA:
            self.frag_buf += self.rc4_in.crypt(pk.payload) if pk.payload else b""
            if len(self.frag_buf) > MAX_MESSAGE_SIZE:
                log.warning("[%s] %s sent an oversized message, dropping", self.server.name, self.addr)
                self.disconnect()
                return
            if pk.fragment == 0:
                msg, self.frag_buf = self.frag_buf, b""
                self.server.on_message(self, msg)
        elif pk.type == DISCONNECT:
            self.close()

    def _multi_ack(self, pk):
        p = pk.payload
        if pk.substream == 1 and len(p) >= 4 and len(p) == 4 + p[1] * 2:     # new format
            base = struct.unpack_from("<H", p, 2)[0]
            extra = struct.unpack_from("<%dH" % p[1], p, 4)
        else:                                                              # old format
            base = pk.seq
            extra = struct.unpack("<%dH" % (len(p) // 2), p[:len(p) // 2 * 2])
        for s in list(self.unacked):
            if ((base - s) & 0xFFFF) < 0x8000 or s in extra:
                self.unacked.pop(s, None)


class PRUDPServer(asyncio.DatagramProtocol):
    """One UDP port. `secure` decides whether CONNECT carries a Kerberos ticket."""

    def __init__(self, name, access_key, on_message, login_handler=None, on_close=None,
                 fragment_size=1300, max_minor=None, functions=0):
        self.name = name
        self.codec = Codec(access_key)
        self.on_message = on_message
        self.login_handler = login_handler
        self.on_close = on_close or (lambda c: None)
        self.fragment_size = fragment_size
        self.max_minor, self.my_functions = max_minor, functions
        self.connections = {}
        self.syns = {}
        self._cid = random.randint(10000, 60000)
        self.transport = None

    def next_cid(self):
        self._cid += 1
        return self._cid

    def connection_made(self, transport):
        self.transport = transport
        asyncio.get_event_loop().create_task(self._maintenance())

    def datagram_received(self, data, addr):
        try:
            for pk in decode(data):
                self._packet(pk, addr)
        except Exception as e:
            log.warning("[%s] bad datagram from %s: %s", self.name, addr, e)

    def _packet(self, pk, addr):
        key = (addr, pk.src)
        if pk.type == SYN and not pk.flags & F_ACK:
            if not self.codec.verify(pk, b"", b""):
                log.debug("[%s] SYN signature mismatch from %s", self.name, addr)
            ack = Packet(SYN, F_ACK)
            ack.src, ack.dst = pk.dst, pk.src
            ack.conn_sig = Codec.connection_signature(addr)
            ack.max_substream = min(0, pk.max_substream)
            ack.minor = pk.minor if self.max_minor is None else min(self.max_minor, pk.minor)   # echo like Pretendo
            ack.functions = self.my_functions & pk.functions
            if len(self.syns) >= MAX_PENDING_SYNS:
                self.syns.clear()
            self.syns[key] = pk
            self.transport.sendto(self.codec.encode(ack, b"", b""), addr)
            log.debug("[%s] SYN %s minor=%d functions=%x", self.name, addr, pk.minor, pk.functions)
            return
        if pk.type == CONNECT and not pk.flags & F_ACK:
            conn = self.connections.get(key)
            if conn is None:
                if len(self.connections) >= MAX_CONNECTIONS:
                    log.warning("[%s] connection limit reached, ignoring %s", self.name, addr)
                    return
                conn = Connection(self, addr, self.syns.pop(key, pk), pk)
                if not self.codec.verify(pk, b"", conn.server_conn_sig):
                    log.debug("[%s] CONNECT signature mismatch from %s", self.name, addr)
                response = b""
                if self.login_handler:
                    try:
                        response = self.login_handler(conn, pk.payload)
                    except Exception as e:
                        log.warning("[%s] secure login rejected for %s: %s", self.name, addr, e)
                        return
                conn.connect_response = response
                self.connections[key] = conn
                log.info("[%s] connection from %s pid=%s minor=%d", self.name, addr, conn.pid, conn.minor)
                if conn.minor >= 3:
                    log.warning("[%s] client negotiated PRUDP minor %d (structure headers) - unexpected for NEX 3.2.1",
                                self.name, conn.minor)
            ack = Packet(CONNECT, F_ACK | F_HAS_SIZE)
            ack.src, ack.dst = pk.dst, pk.src
            ack.session, ack.seq = conn.session, 1
            ack.conn_sig = b"\0" * 16
            ack.max_substream, ack.minor, ack.functions = pk.max_substream, pk.minor, pk.functions
            ack.payload = conn.connect_response
            self.transport.sendto(self.codec.encode(ack, b"", pk.conn_sig), addr)
            return
        conn = self.connections.get(key)
        if conn is None:
            log.debug("[%s] packet for unknown connection %s %r", self.name, addr, pk)
            return
        conn.handle(pk)

    async def _maintenance(self):
        while True:
            await asyncio.sleep(0.5)
            now = time.time()
            for conn in list(self.connections.values()):
                if now - conn.last_seen > 60:
                    log.info("[%s] %s timed out", self.name, conn.addr)
                    conn.close()
                    continue
                for seq, item in list(conn.unacked.items()):
                    if now - item[1] > 1.0:
                        if item[2] >= 10:
                            log.info("[%s] %s stopped acking, dropping", self.name, conn.addr)
                            conn.close()
                            break
                        self.transport.sendto(item[0], conn.addr)
                        item[1], item[2] = now, item[2] + 1
