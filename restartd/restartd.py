#!/usr/bin/env python3
"""restartd - authenticated "restart one service" listener for start.sh.

start.sh supervises every bridge service in its own loop and drops a registry
(run/services.json) listing the services that may be restarted. This listener
accepts signed HTTP requests and turns them into request files
(run/requests/<service>) that the matching supervisor loop picks up. It never
runs shell commands itself and it never touches a process directly.

Independent of relay-admin/nginx on purpose: it keeps working when they are
down or being restarted.

Endpoints (all require a valid signature, see below):
  GET  /v1/services   -> list of services with running/pending/status info
  POST /v1/restart    -> {"service": "wsc-secure", "rebuild": false}

Authentication: HMAC-SHA256 with a shared secret (restartd/secret.key, created
on first start, mode 0600, never logged). A request carries
  X-RD-Timestamp  unix seconds, must be within +-60 s of our clock
  X-RD-Nonce      8-64 chars [A-Za-z0-9], single use inside the window
  X-RD-Signature  hex HMAC-SHA256 over the lines
                  "v1", METHOD, PATH, TIMESTAMP, NONCE, sha256hex(body)
The secret never travels; a captured request cannot be replayed or altered.
There is no TLS here (the payload is not secret), so the only thing an
eavesdropper learns is which service was restarted.
"""

import hashlib
import hmac
import json
import os
import re
import secrets
import subprocess
import sys
import threading
import time
from collections import OrderedDict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
ROOT = HERE.parent
RUN = Path(os.environ.get("RESTARTD_RUN", ROOT / "run"))
BIND = os.environ.get("RESTARTD_BIND", "0.0.0.0")
PORT = int(os.environ.get("RESTARTD_PORT", "9333"))
SECRET_FILE = Path(os.environ.get("RESTARTD_SECRET_FILE", HERE / "secret.key"))

MAX_BODY = 2048
TIMESTAMP_WINDOW = 60          # seconds of clock skew tolerated
NONCE_TTL = 2 * TIMESTAMP_WINDOW
MIN_RESTART_INTERVAL = 20      # per service, seconds
AUTH_FAIL_LIMIT = 10           # failed auths per IP inside AUTH_FAIL_WINDOW ...
AUTH_FAIL_WINDOW = 60
AUTH_BLOCK_SECONDS = 300       # ... blocks that IP for this long

NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,63}$")
NONCE_RE = re.compile(r"^[A-Za-z0-9]{8,64}$")

_lock = threading.Lock()
_nonces: "OrderedDict[str, float]" = OrderedDict()
_last_restart: dict = {}
_auth_fails: dict = {}
_blocked_until: dict = {}


def log(msg: str) -> None:
    print(f"{time.strftime('%Y/%m/%d %H:%M:%S')} restartd: {msg}", flush=True)


def load_secret() -> bytes:
    if not SECRET_FILE.exists():
        SECRET_FILE.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(SECRET_FILE, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as f:
            f.write(secrets.token_hex(32) + "\n")
        log(f"created a new secret at {SECRET_FILE} (mode 0600) - copy it into the PHP page's config")
    secret = SECRET_FILE.read_text().strip()
    if len(secret) < 32:
        log(f"refusing to start: secret in {SECRET_FILE} is shorter than 32 characters")
        sys.exit(1)
    return secret.encode()


def load_registry() -> dict:
    """service name -> {"buildable": bool}, as written by start.sh."""
    try:
        data = json.loads((RUN / "services.json").read_text())
    except (OSError, ValueError):
        return {}
    out = {}
    for item in data.get("services", []):
        name = item.get("name", "")
        if NAME_RE.match(name):
            out[name] = {"buildable": bool(item.get("buildable"))}
    return out


def read_status(name: str) -> dict:
    status = {}
    try:
        for line in (RUN / "status" / name).read_text().splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                status[k.strip()] = v.strip()
    except OSError:
        pass
    return status


def is_running(name: str):
    try:
        pid = int((RUN / "pids" / name).read_text().strip())
        os.kill(pid, 0)
        return pid
    except (OSError, ValueError):
        return None


def sign(secret: bytes, method: str, path: str, ts: str, nonce: str, body: bytes) -> str:
    msg = "\n".join(["v1", method.upper(), path, ts, nonce, hashlib.sha256(body).hexdigest()])
    return hmac.new(secret, msg.encode(), hashlib.sha256).hexdigest()


# ---- per-process usage sampling -------------------------------------------
# A background thread samples every supervised service (its main process plus all
# descendants) from /proc and `ss` every SAMPLE_INTERVAL seconds and keeps the
# latest numbers, so answering a request never blocks on measuring.
#
# What can and cannot be measured without root:
#   CPU, memory, disk read/write   exact, from /proc/<pid>/{stat,io}
#   TCP                            connection count and bytes in/out, from the
#                                  kernel's per-socket counters via `ss -tinp`
#                                  (live connections only - a closed connection's
#                                  bytes disappear, so a rate can dip, never go
#                                  negative)
#   UDP                            socket count only. Linux keeps no per-process
#                                  UDP byte counters, and WSC's game traffic is
#                                  UDP, so per-service UDP throughput would need
#                                  root/eBPF; the host-wide NIC totals are shown
#                                  instead.
SAMPLE_INTERVAL = 3.0
CLK_TCK = os.sysconf("SC_CLK_TCK")
PAGE_SIZE = os.sysconf("SC_PAGE_SIZE")

_metrics: dict = {}
_host: dict = {}
_prev: dict = {}
_prev_host: dict = {}


def read_proc_table() -> dict:
    procs = {}
    for entry in os.listdir("/proc"):
        if not entry.isdigit():
            continue
        try:
            with open(f"/proc/{entry}/stat") as f:
                data = f.read()
        except OSError:
            continue
        # The command name may contain spaces and parentheses: split after the last ")".
        f2 = data[data.rfind(")") + 2:].split()
        try:
            procs[int(entry)] = {
                "ppid": int(f2[1]),
                "ticks": int(f2[11]) + int(f2[12]),   # utime + stime
                "threads": int(f2[17]),
                "rss": int(f2[21]) * PAGE_SIZE,
            }
        except (IndexError, ValueError):
            continue
    return procs


def descendants(root: int, procs: dict) -> list:
    children: dict = {}
    for pid, info in procs.items():
        children.setdefault(info["ppid"], []).append(pid)
    out, todo = [], [root]
    while todo:
        pid = todo.pop()
        if pid in procs:
            out.append(pid)
            todo.extend(children.get(pid, []))
    return out


def read_io(pid: int) -> tuple:
    try:
        vals = {}
        with open(f"/proc/{pid}/io") as f:
            for line in f:
                k, _, v = line.partition(":")
                vals[k] = int(v)
        return vals.get("read_bytes", 0), vals.get("write_bytes", 0)
    except (OSError, ValueError):
        return 0, 0


def ss_lines(flags: str) -> list:
    try:
        out = subprocess.run(["ss", flags, "-H"], capture_output=True, text=True, timeout=5).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    return out.splitlines()


def tcp_by_pid() -> dict:
    """pid -> {"conns": n, "rx": bytes_received, "tx": bytes_sent} for live TCP sockets."""
    result: dict = {}
    current = None
    for line in ss_lines("-tinp"):
        if line[:1].strip():  # a new socket line starts in column 0
            m = re.search(r"pid=(\d+)", line)
            current = int(m.group(1)) if m else None
            if current is not None and not line.startswith("LISTEN"):
                result.setdefault(current, {"conns": 0, "rx": 0, "tx": 0})["conns"] += 1
        elif current is not None and current in result:
            for key, field in (("tx", "bytes_sent"), ("rx", "bytes_received")):
                m = re.search(field + r":(\d+)", line)
                if m:
                    result[current][key] += int(m.group(1))
    return result


def udp_by_pid() -> dict:
    result: dict = {}
    for line in ss_lines("-uanp"):
        m = re.search(r"pid=(\d+)", line)
        if m:
            result[int(m.group(1))] = result.get(int(m.group(1)), 0) + 1
    return result


def read_host() -> dict:
    host: dict = {}
    try:
        with open("/proc/stat") as f:
            cpu = [int(x) for x in f.readline().split()[1:]]
        host["_cpu_total"], host["_cpu_idle"] = sum(cpu), cpu[3] + (cpu[4] if len(cpu) > 4 else 0)
        mem = {}
        with open("/proc/meminfo") as f:
            for line in f:
                k, _, v = line.partition(":")
                mem[k] = int(v.split()[0]) * 1024
        host["mem_total"] = mem.get("MemTotal", 0)
        host["mem_available"] = mem.get("MemAvailable", 0)
        host["load"] = [float(x) for x in open("/proc/loadavg").read().split()[:3]]
        rx = tx = 0
        with open("/proc/net/dev") as f:
            for line in list(f)[2:]:
                iface, _, rest = line.partition(":")
                if iface.strip() == "lo":
                    continue
                cols = rest.split()
                rx += int(cols[0])
                tx += int(cols[8])
        host["_net_rx"], host["_net_tx"] = rx, tx
    except (OSError, ValueError, IndexError):
        pass
    return host


def _rate(now_val, prev_val, dt):
    if prev_val is None or dt <= 0 or now_val < prev_val:
        return None
    return (now_val - prev_val) / dt


def sample_once() -> None:
    global _prev_host
    now = time.time()
    procs = read_proc_table()
    tcp = tcp_by_pid()
    udp = udp_by_pid()
    fresh: dict = {}
    for name in load_registry():
        try:
            main_pid = int((RUN / "pids" / name).read_text().strip())
        except (OSError, ValueError):
            continue
        tree = descendants(main_pid, procs)
        if not tree:
            continue
        cur = {
            "pid": main_pid,
            "time": now,
            "ticks": sum(procs[p]["ticks"] for p in tree),
            "disk_r": 0, "disk_w": 0, "tcp_rx": 0, "tcp_tx": 0,
        }
        for p in tree:
            r, w = read_io(p)
            cur["disk_r"] += r
            cur["disk_w"] += w
            t = tcp.get(p)
            if t:
                cur["tcp_rx"] += t["rx"]
                cur["tcp_tx"] += t["tx"]
        prev = _prev.get(name)
        if prev and prev["pid"] != main_pid:
            prev = None  # the service was restarted: rates restart too
        dt = (now - prev["time"]) if prev else 0
        cpu = _rate(cur["ticks"], prev["ticks"] if prev else None, dt)
        fresh[name] = {
            "processes": len(tree),
            "threads": sum(procs[p]["threads"] for p in tree),
            "cpu_percent": None if cpu is None else round(cpu / CLK_TCK * 100, 1),
            "rss_bytes": sum(procs[p]["rss"] for p in tree),
            "disk_read_bps": _rate(cur["disk_r"], prev["disk_r"] if prev else None, dt),
            "disk_write_bps": _rate(cur["disk_w"], prev["disk_w"] if prev else None, dt),
            "disk_read_total": cur["disk_r"],
            "disk_write_total": cur["disk_w"],
            "tcp_connections": sum(tcp.get(p, {}).get("conns", 0) for p in tree),
            "tcp_rx_bps": _rate(cur["tcp_rx"], prev["tcp_rx"] if prev else None, dt),
            "tcp_tx_bps": _rate(cur["tcp_tx"], prev["tcp_tx"] if prev else None, dt),
            "udp_sockets": sum(udp.get(p, 0) for p in tree),
        }
        _prev[name] = cur
    host = read_host()
    hp = _prev_host
    host_out = {
        "mem_total": host.get("mem_total"), "mem_available": host.get("mem_available"), "load": host.get("load"),
        "cpu_percent": None, "net_rx_bps": None, "net_tx_bps": None,
    }
    if hp and "_cpu_total" in host and host["_cpu_total"] > hp.get("_cpu_total", 0):
        busy = 1 - (host["_cpu_idle"] - hp["_cpu_idle"]) / (host["_cpu_total"] - hp["_cpu_total"])
        host_out["cpu_percent"] = round(busy * 100, 1)
    if hp:
        dt = now - hp.get("_time", now)
        host_out["net_rx_bps"] = _rate(host.get("_net_rx", 0), hp.get("_net_rx"), dt)
        host_out["net_tx_bps"] = _rate(host.get("_net_tx", 0), hp.get("_net_tx"), dt)
    host["_time"] = now
    _prev_host = host
    with _lock:
        _metrics.clear()
        _metrics.update(fresh)
        _host.clear()
        _host.update(host_out)


def sampler_loop() -> None:
    while True:
        try:
            sample_once()
        except Exception as exc:  # never let a measuring hiccup kill the listener
            log(f"sampler error: {exc!r}")
        time.sleep(SAMPLE_INTERVAL)


class Handler(BaseHTTPRequestHandler):
    server_version = "restartd"
    sys_version = ""
    secret: bytes = b""

    def log_message(self, fmt, *args):  # silence the default access log
        pass

    def _send(self, code: int, obj: dict) -> None:
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _client(self) -> str:
        return self.client_address[0]

    def _auth_failed(self, reason: str) -> None:
        ip = self._client()
        now = time.time()
        with _lock:
            fails = [t for t in _auth_fails.get(ip, []) if now - t < AUTH_FAIL_WINDOW] + [now]
            _auth_fails[ip] = fails
            if len(fails) >= AUTH_FAIL_LIMIT:
                _blocked_until[ip] = now + AUTH_BLOCK_SECONDS
                _auth_fails[ip] = []
                log(f"{ip} blocked for {AUTH_BLOCK_SECONDS}s after {AUTH_FAIL_LIMIT} failed authentications")
        log(f"auth failed from {ip}: {reason}")
        # Same answer for every failure: do not tell a prober what was wrong.
        self._send(401, {"ok": False, "error": "unauthorized"})

    def _authenticate(self, body: bytes) -> bool:
        ip = self._client()
        with _lock:
            if _blocked_until.get(ip, 0) > time.time():
                self._send(429, {"ok": False, "error": "too many failed attempts"})
                return False
        ts = self.headers.get("X-RD-Timestamp", "")
        nonce = self.headers.get("X-RD-Nonce", "")
        sig = self.headers.get("X-RD-Signature", "")
        if not (ts.isdigit() and len(ts) <= 12 and NONCE_RE.match(nonce) and re.fullmatch(r"[0-9a-f]{64}", sig)):
            self._auth_failed("malformed authentication headers")
            return False
        if abs(time.time() - int(ts)) > TIMESTAMP_WINDOW:
            self._auth_failed("timestamp outside the allowed window")
            return False
        path = self.path.split("?", 1)[0]
        expected = sign(self.secret, self.command, path, ts, nonce, body)
        if not hmac.compare_digest(expected, sig):
            self._auth_failed("bad signature")
            return False
        now = time.time()
        with _lock:
            while _nonces and next(iter(_nonces.values())) < now:
                _nonces.popitem(last=False)
            if nonce in _nonces:
                replay = True
            else:
                replay = False
                _nonces[nonce] = now + NONCE_TTL
        if replay:
            self._auth_failed("replayed nonce")
            return False
        return True

    def _read_body(self) -> "bytes | None":
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = -1
        if length < 0 or length > MAX_BODY:
            self._send(413, {"ok": False, "error": "body too large"})
            return None
        return self.rfile.read(length) if length else b""

    def do_GET(self):
        body = self._read_body()
        if body is None or not self._authenticate(body):
            return
        path = self.path.split("?", 1)[0]
        if path != "/v1/services":
            return self._send(404, {"ok": False, "error": "not found"})
        services = []
        for name, info in sorted(load_registry().items()):
            pid = is_running(name)
            services.append({
                "name": name,
                "buildable": info["buildable"],
                "running": pid is not None,
                "pid": pid,
                "pending": (RUN / "requests" / name).exists(),
                "status": read_status(name),
                "metrics": _metrics.get(name),
            })
        with _lock:
            host = dict(_host)
        self._send(200, {"ok": True, "services": services, "host": host, "time": int(time.time())})

    def do_POST(self):
        body = self._read_body()
        if body is None or not self._authenticate(body):
            return
        path = self.path.split("?", 1)[0]
        if path != "/v1/restart":
            return self._send(404, {"ok": False, "error": "not found"})
        try:
            req = json.loads(body or b"{}")
            name = req["service"]
            rebuild = bool(req.get("rebuild", False))
        except (ValueError, KeyError, TypeError):
            return self._send(400, {"ok": False, "error": "expected JSON {\"service\": name, \"rebuild\": bool}"})
        if not isinstance(name, str) or not NAME_RE.match(name):
            return self._send(400, {"ok": False, "error": "invalid service name"})
        registry = load_registry()
        if name not in registry:
            log(f"{self._client()} asked to restart unknown service {name!r}")
            return self._send(404, {"ok": False, "error": "unknown service"})
        if rebuild and not registry[name]["buildable"]:
            return self._send(400, {"ok": False, "error": f"{name} has no build step (it is not compiled)"})
        req_file = RUN / "requests" / name
        now = time.time()
        with _lock:
            if req_file.exists():
                return self._send(409, {"ok": False, "error": "a restart is already pending for this service"})
            wait = MIN_RESTART_INTERVAL - (now - _last_restart.get(name, 0))
            if wait > 0:
                return self._send(429, {"ok": False, "error": f"restarted moments ago, try again in {int(wait) + 1}s"})
            _last_restart[name] = now
            tmp = req_file.with_suffix(".tmp")
            tmp.write_text(json.dumps({"rebuild": rebuild, "time": int(now), "from": self._client()}))
            os.replace(tmp, req_file)
        log(f"queued {'rebuild+' if rebuild else ''}restart of {name} for {self._client()}")
        self._send(202, {"ok": True, "service": name, "rebuild": rebuild, "queued": True})


def main() -> None:
    (RUN / "requests").mkdir(parents=True, exist_ok=True)
    Handler.secret = load_secret()
    threading.Thread(target=sampler_loop, daemon=True, name="sampler").start()
    srv = ThreadingHTTPServer((BIND, PORT), Handler)
    srv.daemon_threads = True
    log(f"listening on {BIND}:{PORT}, registry {RUN / 'services.json'}")
    try:
        srv.serve_forever()
    except KeyboardInterrupt:
        pass


if __name__ == "__main__":
    main()
