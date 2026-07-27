#!/usr/bin/env python3
"""
Look up a PNID (and Mii name) for a given PID by calling GetBasicInfo on
Pretendo's friends NEX server, authenticated as a known local user.

Prints JSON: {"pid": <int>, "pnid": "<str>", "mii_name": "<str>"}
Exits non-zero on failure.
"""

import argparse
import asyncio
import json
import sys

import psycopg2
from nintendo.nex import backend, settings as nex_settings, friends as friends_lib
from nintendo.nex import rmc as _rmc_module

_orig_rmc_init = _rmc_module.RMCClient.__init__
def _patched_rmc_init(self, settings, client):
    _orig_rmc_init(self, settings, client)
    self.settings["nex.struct_header"] = False
_rmc_module.RMCClient.__init__ = _patched_rmc_init

ACCESS_KEY = "ridfebb9"
NEX_VERSION = 31011


async def lookup(target_pid: int, caller_pid: int, nex_password: str,
                 auth_host: str, auth_port: int, db_uri: str):
    s = nex_settings.default()
    s["prudp.access_key"] = ACCESS_KEY
    s["nex.version"] = NEX_VERSION
    s["kerberos.key_size"] = 16

    async with backend.connect(s, auth_host, auth_port) as be:
        async with be.login(str(caller_pid), nex_password) as client:
            friends_client = friends_lib.FriendsClientV2(client)
            results = await friends_client.get_basic_info([target_pid])

    if not results:
        print(f"GetBasicInfo returned no results for PID {target_pid}", file=sys.stderr)
        sys.exit(1)

    info = results[0]
    pnid = info.nnid or ""
    mii_name = info.mii.name if info.mii else ""

    if pnid:
        conn = psycopg2.connect(db_uri)
        cur = conn.cursor()
        cur.execute("""
            INSERT INTO pnid_cache (pid, pnid)
            VALUES (%s, %s)
            ON CONFLICT (pid) DO UPDATE SET pnid = EXCLUDED.pnid, updated_at = NOW()
        """, (target_pid, pnid))
        if mii_name:
            cur.execute("""
                INSERT INTO mii_names (pid, mii_name)
                VALUES (%s, %s)
                ON CONFLICT (pid) DO UPDATE SET mii_name = EXCLUDED.mii_name
            """, (target_pid, mii_name))
        conn.commit()
        cur.close()
        conn.close()

    print(json.dumps({"pid": target_pid, "pnid": pnid, "mii_name": mii_name}))


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--target-pid", type=int, required=True)
    parser.add_argument("--caller-pid", type=int, required=True)
    parser.add_argument("--nex-password", required=True)
    parser.add_argument("--auth-host", required=True)
    parser.add_argument("--auth-port", type=int, required=True)
    parser.add_argument("--db-uri", required=True)
    args = parser.parse_args()

    asyncio.run(lookup(
        args.target_pid, args.caller_pid, args.nex_password,
        args.auth_host, args.auth_port, args.db_uri,
    ))


if __name__ == "__main__":
    main()
