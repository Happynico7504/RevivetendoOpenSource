#!/usr/bin/env python3
"""
Fetch Mii data (FFLStoreData) for a given PID or PNID via Pretendo's Friends NEX server.

PID mode  (--target-pid):  calls GetBasicInfo([pid])
PNID mode (--target-pnid): calls add_friend_by_name(pnid) then immediately cancels
                            the request, giving us Mii data for any PNID without
                            needing to know their PID first.

Prints JSON: {"pid": <int>, "pnid": "<str>", "mii_name": "<str>", "mii_data": "<hex>"}
Exits non-zero on failure.
"""

import argparse
import asyncio
import json
import sys

from nintendo.nex import backend, settings as nex_settings, friends as friends_lib
from nintendo.nex import rmc as _rmc_module

_orig_rmc_init = _rmc_module.RMCClient.__init__
def _patched_rmc_init(self, settings, client):
    _orig_rmc_init(self, settings, client)
    self.settings["nex.struct_header"] = False
_rmc_module.RMCClient.__init__ = _patched_rmc_init

ACCESS_KEY = "ridfebb9"
NEX_VERSION = 31011


async def fetch_mii_by_pid(target_pid: int, caller_pid: int, nex_password: str,
                           auth_host: str, auth_port: int):
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
    mii_data = bytes(info.mii.data) if (info.mii and info.mii.data) else b""

    if not mii_data:
        print(f"No mii data in GetBasicInfo response for PID {target_pid}", file=sys.stderr)
        sys.exit(1)

    print(json.dumps({
        "pid":      target_pid,
        "pnid":     pnid,
        "mii_name": mii_name,
        "mii_data": mii_data.hex(),
    }))


async def fetch_mii_by_pnid(target_pnid: str, caller_pid: int, nex_password: str,
                             auth_host: str, auth_port: int):
    s = nex_settings.default()
    s["prudp.access_key"] = ACCESS_KEY
    s["nex.version"] = NEX_VERSION
    s["kerberos.key_size"] = 16

    async with backend.connect(s, auth_host, auth_port) as be:
        async with be.login(str(caller_pid), nex_password) as client:
            friends_client = friends_lib.FriendsClientV2(client)

            response = await friends_client.add_friend_by_name(target_pnid)

            # Immediately cancel so the target never actually receives the request.
            req_id = int(response.request.message.friend_request_id)
            try:
                await friends_client.cancel_friend_request(req_id)
            except Exception as e:
                print(f"warning: cancel_friend_request({req_id}) failed: {e}", file=sys.stderr)

    info = response.info
    pid = int(info.nna_info.principal_info.pid)
    pnid = info.nna_info.principal_info.nnid or target_pnid
    mii = info.nna_info.principal_info.mii
    mii_name = mii.name if mii else ""
    mii_data = bytes(mii.data) if (mii and mii.data) else b""

    if not mii_data:
        print(f"No mii data in add_friend_by_name response for PNID {target_pnid}", file=sys.stderr)
        sys.exit(1)

    print(json.dumps({
        "pid":      pid,
        "pnid":     pnid,
        "mii_name": mii_name,
        "mii_data": mii_data.hex(),
    }))


def main():
    parser = argparse.ArgumentParser()
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--target-pid", type=int)
    group.add_argument("--target-pnid", type=str)
    parser.add_argument("--caller-pid", type=int, required=True)
    parser.add_argument("--nex-password", required=True)
    parser.add_argument("--auth-host", required=True)
    parser.add_argument("--auth-port", type=int, required=True)
    args = parser.parse_args()

    if args.target_pid is not None:
        asyncio.run(fetch_mii_by_pid(
            args.target_pid, args.caller_pid, args.nex_password,
            args.auth_host, args.auth_port,
        ))
    else:
        asyncio.run(fetch_mii_by_pnid(
            args.target_pnid, args.caller_pid, args.nex_password,
            args.auth_host, args.auth_port,
        ))


if __name__ == "__main__":
    main()
