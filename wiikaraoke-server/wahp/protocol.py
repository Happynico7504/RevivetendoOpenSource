"""
Wire format of the Wii Karaoke U by JOYSOUND (EU, WAHP) game server.

Everything here was recovered from app.rpx (EU 3.0, "trunk_eu_3_0"):

REQUEST  (game -> server)
  GET  https://ssl.wahp.wah.wup.app.nintendo.net/control_Orchestra300_EU.php?type=..&k=v..
  POST same URL, body "k=v&k=v.."  (only the SendLog / LG000x commands)

  Every *value* is base64-encoded and then URL-encoded by the game
  (flag bits 8|16 of sub_02113468 / sub_02113d38). Keys are plain.
  Common params appended to every request by sub_02111e58:
      cid  country id   (%d, from the game's config block)
      lid  language id  (%d)
      atk  nn::act independent service token (client id 471ac6f9b1368cba77a15da69d5b0241)
      ver  "300"
      gcd  "0"

RESPONSE (server -> game)  -- parsed by the cGame*RecvData classes (0x0213dxxx)
  A *binary* stream (reader at sub_02018b74, big-endian) that mixes raw
  integers with text lines.  A "line" is   urlencode(base64(value)) + "\\r\\n"
  (sub_0213dde0: read to CRLF -> URL-decode -> base64-decode).

  Header (sub_0213df60):
      u8  err_category     0 = normal reply
      u8  result / msglen  if err_category == 0 : 1 = OK, 2 = "no record", other = error
                           if err_category != 0 : >0 means an error-message line follows
      [line errmsg]        only when err_category != 0 and byte1 > 0
  Body depends on the command (see handlers.py).
"""
import base64
import struct
import urllib.parse
from datetime import datetime

TIME_FMT = "%Y%m%d%H%M%S"   # parsed by sub_02113618 (strtoll, /100 split) -> OSCalendarTime


def b64_value(raw: str) -> str:
    """Decode one request value (base64 after the URL layer was removed)."""
    if raw is None:
        return None
    s = raw.strip().replace(" ", "+")         # tolerate '+' that a proxy turned into ' '
    s += "=" * (-len(s) % 4)
    try:
        return base64.b64decode(s, validate=False).decode("utf-8", "replace")
    except Exception:
        return raw


def decode_params(multidict) -> dict:
    return {k: b64_value(v) for k, v in multidict.items()}


def game_time(dt: datetime) -> str:
    return dt.strftime(TIME_FMT)


class Reply:
    """Builds a response body exactly as the game's RecvData parsers expect."""

    def __init__(self):
        self.buf = bytearray()

    # --- primitives --------------------------------------------------------
    def u8(self, v):
        self.buf += struct.pack(">B", v & 0xFF); return self

    def u16(self, v):
        self.buf += struct.pack(">H", v & 0xFFFF); return self

    def u32(self, v):
        self.buf += struct.pack(">I", v & 0xFFFFFFFF); return self

    def line(self, value, maxlen=None):
        b = value.encode("utf-8") if isinstance(value, str) else bytes(value)
        if maxlen is not None and len(b) > maxlen:
            raise ValueError("line too long for game buffer (%d > %d)" % (len(b), maxlen))
        enc = base64.b64encode(b).decode("ascii")
        self.buf += urllib.parse.quote(enc, safe="").encode("ascii") + b"\r\n"
        return self

    # --- headers -----------------------------------------------------------
    def ok(self):
        return self.u8(0).u8(1)

    def no_record(self):
        """result 2 -> game error 20 (e.g. MSG_DIALOG_ERROR_MUSIC_NO_RECORD)."""
        return self.u8(0).u8(2)

    def fail(self, code=3):
        """any other result -> game error 22."""
        return self.u8(0).u8(code)

    def error_message(self, category, message):
        """err_category != 0. category 2 clears the ticket state, 7 = numeric
        Nintendo error code string (e.g. '1093550')."""
        self.u8(category).u8(1)
        return self.line(message, 512)

    def bytes(self):
        return bytes(self.buf)
