"""
Who is calling?  The only per-user value the game sends is `atk`, the
nn::act independent service token for client id 471ac6f9b1368cba77a15da69d5b0241.

Nintendo's tokens were opaque, but a Pretendo(-compatible) account server
issues  base64( u32 pid | u64 title_id | u64 issued_ms | u64 expires_ms | HMAC-SHA256 )
(all big endian, HMAC keyed with the server entry's `aes_key`).
"""
import base64
import hashlib
import hmac
import struct
import time


def decode_service_token(atk, key=""):
    """-> dict(pid, title_id, issued, expires, verified) or None"""
    if not atk:
        return None
    try:
        raw = base64.b64decode(atk + "=" * (-len(atk) % 4))
    except Exception:
        return None
    if len(raw) != 60:
        return None
    data, mac = raw[:28], raw[28:]
    pid, title, issued, expires = struct.unpack(">IQQQ", data)
    verified = None
    if key:
        verified = hmac.compare_digest(hmac.new(key.encode(), data, hashlib.sha256).digest(), mac)
    return {"pid": pid, "title_id": "%016X" % title, "issued": issued / 1000,
            "expires": expires / 1000, "verified": verified}


def client_ip(request, cfg):
    ip = request.remote_addr or "0.0.0.0"
    if cfg["trust_proxy_headers"] and request.headers.get("X-Forwarded-For"):
        ip = request.headers["X-Forwarded-For"].split(",")[0].strip()
    return ip


def game_account_key(request, cfg, params):
    """Account key for a game request -> (key, note)."""
    if cfg["identity"] == "single":
        return "default", ""
    if cfg["identity"] == "token":
        tok = decode_service_token(params.get("atk"), cfg.get("service_token_key", ""))
        if tok:
            if tok["verified"] is False:
                return None, "service token HMAC mismatch"
            if tok["expires"] and tok["expires"] < time.time() - 300:
                return None, "service token expired"
            return "pid:%d" % tok["pid"], "title %s" % tok["title_id"]
    return "ip:" + client_ip(request, cfg), ""
