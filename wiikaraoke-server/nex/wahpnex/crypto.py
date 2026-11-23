"""RC4 + Kerberos helpers (pure Python, no third-party crypto needed)."""
import hashlib
import hmac
import os
import struct

from .streams import StreamIn, StreamOut, nex_datetime, nex_datetime_to_ts


class RC4:
    """Stateful RC4 stream (PRUDP keeps one stream per direction)."""

    def __init__(self, key):
        S = list(range(256))
        j = 0
        for i in range(256):
            j = (j + S[i] + key[i % len(key)]) & 0xFF
            S[i], S[j] = S[j], S[i]
        self.S, self.i, self.j = S, 0, 0

    def crypt(self, data):
        S, i, j = self.S, self.i, self.j
        out = bytearray(len(data))
        for n, b in enumerate(data):
            i = (i + 1) & 0xFF
            j = (j + S[i]) & 0xFF
            S[i], S[j] = S[j], S[i]
            out[n] = b ^ S[(S[i] + S[j]) & 0xFF]
        self.i, self.j = i, j
        return bytes(out)


def derive_key(password: bytes, pid: int) -> bytes:
    """3DS/Wii U: MD5 iterated 65000 + pid % 1024 times."""
    key = password
    for _ in range(65000 + pid % 1024):
        key = hashlib.md5(key).digest()
    return key


def kerberos_encrypt(key: bytes, data: bytes) -> bytes:
    enc = RC4(key).crypt(data)
    return enc + hmac.digest(key, enc, "md5")


def kerberos_decrypt(key: bytes, data: bytes) -> bytes:
    body, mac = data[:-16], data[-16:]
    if not hmac.compare_digest(hmac.digest(key, body, "md5"), mac):
        raise ValueError("kerberos checksum mismatch (wrong key/password)")
    return RC4(key).crypt(body)


def make_ticket(user_key: bytes, user_pid: int, server_key: bytes, server_pid: int = 2, key_size: int = 32):
    """-> (encrypted ticket for the client, session key)."""
    session_key = os.urandom(key_size)
    internal = StreamOut().datetime(nex_datetime()).pid(user_pid).write(session_key).bytes()
    internal_enc = kerberos_encrypt(server_key, internal)          # "old" internal ticket format
    ticket = StreamOut().write(session_key).pid(server_pid).buffer(internal_enc).bytes()
    return kerberos_encrypt(user_key, ticket), session_key


def open_internal_ticket(server_key: bytes, data: bytes, key_size: int = 32):
    """-> (issued unix ts, source pid, session key)."""
    s = StreamIn(kerberos_decrypt(server_key, data))
    issued = s.datetime()
    pid = s.pid()
    return nex_datetime_to_ts(issued), pid, s.read(key_size)
