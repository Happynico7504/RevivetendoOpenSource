#!/usr/bin/env python3
"""
Console-side NEX client for testing the server without a Wii U.

Does what Wii Karaoke U does: PRUDP v1 handshake (verifying the server's
signatures the way the client library does), TicketGranting LoginEx with an
AuthenticationInfo, secure connection with the Kerberos ticket, RegisterEx,
Utility AcquireNexUniqueId, Ranking UploadCommonData / UploadScore /
GetRanking / GetApproxOrder / GetStats / GetRankingByPIDList.

  python3 tools/nex_test_client.py 127.0.0.1 60000 <pid> <nex password>
"""
import hashlib
import hmac
import os
import random
import socket
import struct
import sys

sys.path.insert(0, os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
from wahpnex.crypto import RC4, derive_key, kerberos_decrypt, kerberos_encrypt
from wahpnex.prudp import decode, Packet, SYN, CONNECT, DATA, DISCONNECT, F_ACK, F_RELIABLE, F_NEED_ACK, F_HAS_SIZE
from wahpnex.streams import StreamIn, StreamOut, parse_station_url

ACCESS_KEY = b"dfc5a4ac"


def sign(pk, session_key, conn_sig):
    """Independent implementation of the PRUDP v1 signature (per kinnay's client)."""
    opts = pk.options()
    hdr = pk.header(len(opts))
    m = hmac.new(hashlib.md5(ACCESS_KEY).digest(), digestmod=hashlib.md5)
    m.update(hdr[4:]); m.update(session_key); m.update(struct.pack("<I", sum(ACCESS_KEY)))
    m.update(conn_sig); m.update(opts); m.update(pk.payload)
    return m.digest()


def client_conn_sig(addr):
    key = bytes.fromhex("26c31f381e46d6eb38e1af6ab70d11")
    return hmac.digest(key, socket.inet_aton(addr[0]) + struct.pack(">H", addr[1]), "md5")


class Client:
    def __init__(self, host, port, session_key=b"", login_payload=None):
        self.addr = (host, port)
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        self.sock.settimeout(3)
        self.session_key = session_key
        key = session_key or b"CD&ML"
        self.rc4_out, self.rc4_in = RC4(key), RC4(key)
        self.seq = 1
        self.expect = 1
        self.session = random.randint(1, 255)
        self.call_id = 1
        self.verified = 0
        self._handshake(login_payload or b"")

    def _send(self, pk, sig_key, conn_sig):
        pk.src, pk.dst = 0xAF, 0xA1
        pk.signature = sign(pk, sig_key, conn_sig)
        opts = pk.options()
        self.sock.sendto(b"\xEA\xD0" + pk.header(len(opts)) + pk.signature + opts + pk.payload, self.addr)

    def _recv(self):
        data, _ = self.sock.recvfrom(4096)
        return decode(data)

    def _handshake(self, login_payload):
        syn = Packet(SYN, F_NEED_ACK); syn.minor = 2; syn.max_substream = 0
        self._send(syn, b"", b"")
        ack = self._recv()[0]
        assert ack.type == SYN and ack.flags & F_ACK
        assert ack.signature == sign(ack, b"", b""), "SYN/ACK signature invalid"
        self.server_sig = ack.conn_sig
        self.minor = ack.minor
        con = Packet(CONNECT, F_RELIABLE | F_NEED_ACK | F_HAS_SIZE)
        con.session = self.session; con.seq = 1; con.minor = self.minor
        con.conn_sig = client_conn_sig(self.addr); con.unrel_seq = random.randint(0, 0xFFFF)
        con.payload = login_payload
        self.my_sig = con.conn_sig
        self._send(con, b"", self.server_sig)
        cack = self._recv()[0]
        assert cack.type == CONNECT and cack.flags & F_ACK and cack.seq == 1
        assert cack.signature == sign(cack, b"", self.my_sig), "CONNECT/ACK signature invalid"
        self.connect_response = cack.payload
        self.server_session = cack.session
        self.seq = 2

    def rmc(self, proto, method, params=b""):
        cid = self.call_id; self.call_id += 1
        body = struct.pack("<BII", proto | 0x80, cid, method) + params
        msg = struct.pack("<I", len(body)) + body
        pk = Packet(DATA, F_RELIABLE | F_NEED_ACK | F_HAS_SIZE)
        pk.session, pk.seq, pk.fragment = self.session, self.seq, 0
        self.seq += 1
        pk.payload = self.rc4_out.crypt(msg)
        self._send(pk, self.session_key, self.server_sig)
        buf = b""
        while True:
            for r in self._recv():
                if r.flags & F_ACK:
                    continue
                assert r.signature == sign(r, self.session_key, self.my_sig), "DATA signature invalid"
                self.verified += 1
                a = Packet(r.type, F_ACK); a.session = self.session; a.seq = r.seq; a.fragment = r.fragment
                self._send(a, self.session_key, self.server_sig)
                if r.type == DATA:
                    assert r.seq == self.expect, "out of order"
                    self.expect += 1
                    buf += self.rc4_in.crypt(r.payload)
                    if r.fragment == 0:
                        s = StreamIn(buf)
                        s.u32(); assert s.u8() == proto
                        if s.u8() == 1:
                            assert s.u32() == cid and s.u32() == method | 0x8000
                            return s
                        code = s.u32()
                        raise RuntimeError("RMC error 0x%08X" % code)

    def disconnect(self):
        pk = Packet(DISCONNECT, F_RELIABLE | F_NEED_ACK); pk.session = self.session; pk.seq = self.seq
        self._send(pk, self.session_key, self.server_sig)


def any_data(name, payload):
    return StreamOut().string(name).u32(len(payload) + 4).buffer(payload).bytes()


def order_param(offset=0, length=10, calc=0, gidx=255, gnum=0, scope=2):
    return struct.pack("<BBBBIB", calc, gidx, gnum, scope, offset, length)


def login(host, port, pid, password, cid_group=110):
    auth = Client(host, port)
    info = StreamOut().string("NEXTOKEN-TEST").u32(3).u8(1).u32(2).bytes()
    s = auth.rmc(10, 2, StreamOut().string(str(pid)).write(any_data("AuthenticationInfo", info)).bytes())
    result, rpid, ticket = s.u32(), s.pid(), s.buffer()
    url, specials, url2, build = s.string(), s.list(lambda x: x.u8()), s.string(), s.string()
    assert result == 0x10001, hex(result)
    t = StreamIn(kerberos_decrypt(derive_key(password.encode(), pid), ticket))
    session_key, target, internal = t.read(32), t.pid(), t.buffer()
    print("  auth ok: pid=%d secure=%s build=%r verified=%d" % (rpid, url, build, auth.verified))
    auth.disconnect()
    _, p = parse_station_url(url)
    check = random.randint(0, 0xFFFFFFFF)
    req = kerberos_encrypt(session_key, StreamOut().pid(pid).u32(1).u32(check).bytes())
    sec = Client(p["address"], int(p["port"]), session_key, StreamOut().buffer(internal).buffer(req).bytes())
    assert sec.connect_response == struct.pack("<II", 4, (check + 1) & 0xFFFFFFFF), "bad connection check"
    urls = ["prudp:/address=192.168.1.50;port=55555;natf=0;natm=0;pmp=0;sid=15;type=2;upnp=0"]
    s = sec.rmc(11, 4, StreamOut().list(urls, lambda o, u: o.string(u))
                .write(any_data("NintendoLoginData", StreamOut().string("NEXTOKEN-TEST").bytes())).bytes())
    print("  RegisterEx:", hex(s.u32()), s.u32(), s.string())
    return sec


def main():
    host, port = sys.argv[1], int(sys.argv[2])
    users = [(int(sys.argv[3]), sys.argv[4])] if len(sys.argv) > 4 else [(1001, "pw1001"), (1002, "pw1002"), (1003, "pw1003")]
    song = 750001
    for n, (pid, pw) in enumerate(users):
        print("== user", pid)
        sec = login(host, port, pid, pw)
        uid = sec.rmc(110, 1).u64()
        print("  unique id %x" % uid)
        common = b"MIIDATA" + pid.to_bytes(4, "little") + b"\0" * 60
        sec.rmc(112, 4, StreamOut().qvector_u8(common).u64(uid).bytes())
        score = [87123, 92500, 87123][n % 3]
        sd = StreamOut().u32(song).u32(score).u8(1).u8(0).qvector_u8(bytes([110 if n != 1 else 78, 0])).u64(0).u64(uid).bytes()
        sec.rmc(112, 1, sd)
        print("  uploaded score", score)
        s = sec.rmc(112, 9, struct.pack("<BI", 4, song) + order_param(calc=0) + struct.pack("<QI", uid, pid))
        cnt = s.u32()
        print("  own rank:", [(s.pid(), s.u64(), s.u32(), s.u32(), s.u32(), s.qvector_u8().hex(), s.u64(), len(s.qvector_u8())) for _ in range(cnt)][0][2:5])
        sec.disconnect()
    sec = login(host, port, *users[0])
    s = sec.rmc(112, 9, struct.pack("<BI", 0, song) + order_param(calc=0) + struct.pack("<QI", 0, 0))
    cnt = s.u32()
    rows = []
    for _ in range(cnt):
        rows.append((s.pid(), s.u64(), s.u32(), s.u32(), s.u32(), s.qvector_u8().hex(), s.u64(), s.qvector_u8()[:7]))
    print("  global top (1224):", [(r[0], r[2], r[4]) for r in rows], "total", s.u32())
    s = sec.rmc(112, 9, struct.pack("<BI", 0, song) + order_param(calc=1, gidx=0, gnum=110) + struct.pack("<QI", 0, 0))
    cnt = s.u32()
    print("  UK only (1234):", [(s.pid(), s.u64(), s.u32(), s.u32(), s.u32(), s.qvector_u8(), s.u64(), s.qvector_u8())[2] for _ in range(cnt)])
    print("  approx order for 90000:", sec.rmc(112, 10, struct.pack("<I", song) + order_param() + struct.pack("<IQI", 90000, 0, 0)).u32())
    s = sec.rmc(112, 11, struct.pack("<I", song) + order_param() + struct.pack("<I", 0x1F))
    print("  stats:", [struct.unpack("<d", s.read(8))[0] for _ in range(s.u32())])
    s = sec.rmc(112, 12, StreamOut().list([users[0][0], users[-1][0]], lambda o, p: o.pid(p)).bytes()
                + struct.pack("<BI", 0, song) + order_param() + struct.pack("<Q", 0))
    print("  by pid list count:", s.u32())
    try:
        sec.rmc(112, 9, struct.pack("<BI", 0, 999) + order_param() + struct.pack("<QI", 0, 0))
    except RuntimeError as e:
        print("  empty category ->", e)
    sec.disconnect()
    print("signatures verified OK")


if __name__ == "__main__":
    main()
