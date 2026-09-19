#!/usr/bin/env python3
"""Drive a PRUDP endpoint like a WSC console would, without a console.

Forges the Kerberos ticket a real login would produce (we know the server password),
connects, optionally sends RMC calls, and reports latency. Used to test the WSC edge
relay (relayd wscedge) and to compare it against the direct secure server.

  WSC_KERBEROS_PASSWORD=... edge_probe.py HOST PORT [--pid N] [--calls K]
"""
import argparse, asyncio, datetime, os, secrets, time

from nintendo.nex import kerberos, rmc, settings as nex_settings, common

ap = argparse.ArgumentParser()
ap.add_argument("host")
ap.add_argument("port", type=int)
ap.add_argument("--pid", type=int, default=1999000001)  # fake PID: never a real user
ap.add_argument("--calls", type=int, default=3)
ap.add_argument("--proto", type=lambda x: int(x, 0), default=0x70)
ap.add_argument("--method", type=lambda x: int(x, 0), default=0x1)
ap.add_argument("--hold", type=float, default=0, help="stay connected this many seconds after the calls")
a = ap.parse_args()

pw = os.environ["WSC_KERBEROS_PASSWORD"].encode()

s = nex_settings.default()
s["prudp.access_key"] = "4d324052"
s["prudp.version"] = 1
s["prudp.minor_version"] = 3
s["nex.version"] = 30400
s["kerberos.key_size"] = 32  # wsc-secure / nex-go default
s["kerberos.ticket_version"] = 0  # nex-go v1.0.16 expects the plain ticket format
s["kerberos.key_derivation"] = 0
s["nex.struct_header"] = False

# Server side of the ticket, encrypted with the key nex-go derives for the secure server (pid 2).
session_key = secrets.token_bytes(32)
server_ticket = kerberos.ServerTicket()
server_ticket.timestamp = common.DateTime.now()
server_ticket.source = a.pid
server_ticket.session_key = session_key
key = kerberos.KeyDerivationOld().derive_key(pw, 2)
internal = server_ticket.encrypt(key, s)

client_ticket = kerberos.ClientTicket()
client_ticket.session_key = session_key
client_ticket.target = 2
client_ticket.internal = internal
creds = kerberos.Credentials(client_ticket, a.pid, 1)


async def main():
    t0 = time.monotonic()
    async with rmc.connect(s, a.host, a.port, 1, None, creds) as client:
        print(f"connected in {(time.monotonic()-t0)*1000:.0f} ms")
        for i in range(a.calls):
            t = time.monotonic()
            try:
                # raw RMC call: empty parameters, we only care that a response comes back
                resp = await asyncio.wait_for(client.request(a.proto, a.method, b""), 10)
                print(f"call {i}: proto={a.proto:#x} method={a.method:#x} -> {len(resp)} bytes in {(time.monotonic()-t)*1000:.0f} ms")
            except Exception as e:
                print(f"call {i}: FAILED {type(e).__name__}: {e}")
        if a.hold:
            await asyncio.sleep(a.hold)
    print("closed cleanly")

asyncio.run(main())
