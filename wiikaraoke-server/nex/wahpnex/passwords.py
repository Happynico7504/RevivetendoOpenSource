"""
Where the NEX server gets each user's NEX password.

On Wii U the console receives (pid, nex_password, host, port) from the
account server's /v1/api/provider/nex_token/@me?game_server_id=10100D01.
The game server must know the same password to build the Kerberos ticket,
so it has to share it with the account server.

Providers:
  pretendo  - Pretendo account server gRPC  account.Account/GetNEXPassword
              (needs `pip install grpcio`; API key from the account server's config)
  json      - a JSON file {"<pid>": "<nex password>", ...} kept in sync by your
              own account server
  static    - one password for everybody (only for a private account server that
              hands out that same password)
"""
import json
import logging
import os

log = logging.getLogger("passwords")


def _pb_uint32_field1(v):
    out = bytearray([0x08])
    while True:
        b = v & 0x7F; v >>= 7
        out.append(b | (0x80 if v else 0))
        if not v:
            return bytes(out)


def _pb_string_field1(data):
    """parse {string password = 1;} from a protobuf message."""
    p = 0
    while p < len(data):
        tag = data[p]; p += 1
        ln = 0; shift = 0
        while True:
            b = data[p]; p += 1
            ln |= (b & 0x7F) << shift; shift += 7
            if not b & 0x80:
                break
        if tag == 0x0A:
            return data[p:p + ln].decode()
        p += ln
    return None


class PretendoGRPC:
    def __init__(self, host, port, api_key, method="/account.Account/GetNEXPassword"):
        import grpc                                   # optional dependency
        self.channel = grpc.insecure_channel("%s:%d" % (host, port))
        self.call = self.channel.unary_unary(method,
                                             request_serializer=lambda b: b,
                                             response_deserializer=lambda b: b)
        self.md = [("x-api-key", api_key)]

    def __call__(self, pid):
        try:
            return _pb_string_field1(self.call(_pb_uint32_field1(pid), metadata=self.md, timeout=5))
        except Exception as e:
            log.warning("gRPC GetNEXPassword(%d) failed: %s", pid, e)
            return None


class JsonFile:
    def __init__(self, path):
        self.path = path

    def __call__(self, pid):
        try:
            with open(self.path, "r", encoding="utf-8") as f:
                return json.load(f).get(str(pid))
        except FileNotFoundError:
            return None


class Static:
    def __init__(self, password):
        self.password = password

    def __call__(self, pid):
        return self.password


def make(cfg):
    kind = cfg.get("password_provider", "json")
    if kind == "pretendo":
        p = cfg["pretendo_grpc"]
        return PretendoGRPC(p["host"], p["port"], p["api_key"],
                            p.get("method", "/account.Account/GetNEXPassword"))
    if kind == "static":
        return Static(cfg["static_password"])
    return JsonFile(cfg.get("password_file", "nex_passwords.json"))
