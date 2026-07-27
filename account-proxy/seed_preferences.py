#!/usr/bin/env python3
"""
One-time script: pull PrincipalPreference and Comment from Pretendo's Friends NEX
for every user in nex_accounts and seed user_settings in our DB.

Requires at least one successful fetch_friends.py run per user so that
last_auth_host / last_auth_port are stored in nex_accounts.
Users missing auth info are skipped (connect their Wii U once to populate it).
"""

import asyncio
import argparse
import sys

import psycopg2
from nintendo.nex import backend, settings as nex_settings, friends as friends_lib
from nintendo.nex import common, rmc as _rmc_module

_orig_rmc_init = _rmc_module.RMCClient.__init__
def _patched_rmc_init(self, settings, client):
    _orig_rmc_init(self, settings, client)
    self.settings["nex.struct_header"] = False
_rmc_module.RMCClient.__init__ = _patched_rmc_init

ACCESS_KEY = "ridfebb9"
NEX_VERSION = 31011


def build_minimal_args():
    nna_info = friends_lib.NNAInfo()
    nna_info.principal_info = friends_lib.PrincipalBasicInfo()
    nna_info.principal_info.pid = 0
    nna_info.principal_info.nnid = ""
    nna_info.principal_info.mii = friends_lib.MiiV2()
    nna_info.principal_info.mii.name = ""
    nna_info.principal_info.mii.unk1 = 0
    nna_info.principal_info.mii.unk2 = 0
    nna_info.principal_info.mii.data = b""
    nna_info.principal_info.mii.datetime = common.DateTime(0)
    nna_info.principal_info.unk = 2
    nna_info.unk1 = 94
    nna_info.unk2 = 11

    presence = friends_lib.NintendoPresenceV2()
    presence.flags = 0
    presence.is_online = False
    presence.game_key = friends_lib.GameKey()
    presence.game_key.title_id = 0
    presence.game_key.title_version = 0
    presence.unk1 = 0
    presence.message = ""
    presence.unk2 = 0
    presence.unk3 = 0
    presence.game_server_id = 0
    presence.unk4 = 0
    presence.pid = 0
    presence.gathering_id = 0
    presence.application_data = b""
    presence.unk5 = 3
    presence.unk6 = 3
    presence.unk7 = 3

    return nna_info, presence, common.DateTime(0)


async def seed_user(pid: int, nex_password: str, auth_host: str, auth_port: int, db_uri: str):
    s = nex_settings.default()
    s["prudp.access_key"] = ACCESS_KEY
    s["nex.version"] = NEX_VERSION
    s["kerberos.key_size"] = 16

    print(f"[{pid}] connecting to {auth_host}:{auth_port} ...", flush=True)
    try:
        async with backend.connect(s, auth_host, auth_port) as be:
            async with be.login(str(pid), nex_password) as client:
                friends_client = friends_lib.FriendsClientV2(client)
                nna_info, presence, birthday = build_minimal_args()
                nna_info.principal_info.pid = pid
                response = await friends_client.update_and_get_all_information(
                    nna_info, presence, birthday
                )
                pref = response.principal_preference
                comment = response.comment

        conn = psycopg2.connect(db_uri)
        cur = conn.cursor()
        # Check if we already have a local row.
        cur.execute("SELECT 1 FROM user_settings WHERE pid = %s", (pid,))
        has_local = cur.fetchone() is not None
        if has_local:
            print(f"[{pid}] local settings exist — pushing to Pretendo skipped (run fetch_friends to push)", flush=True)
        else:
            changed_ts = comment.changed.standard_datetime() if (comment.changed and comment.changed.value() != 0) else None
            cur.execute("""
                INSERT INTO user_settings
                    (pid, show_online_presence, show_current_title, block_friend_requests,
                     comment_unknown, comment_text, comment_changed_at)
                VALUES (%s, %s, %s, %s, %s, %s, COALESCE(%s, NOW()))
                ON CONFLICT (pid) DO NOTHING
            """, (
                pid,
                bool(pref.show_online_status),
                bool(pref.show_current_title),
                bool(pref.block_friend_requests),
                int(comment.unk or 0),
                comment.text or "",
                changed_ts,
            ))
            print(f"[{pid}] seeded: online={pref.show_online_status} title={pref.show_current_title} "
                  f"block={pref.block_friend_requests} comment={comment.text!r}", flush=True)
        conn.commit()
        cur.close()
        conn.close()
    except Exception as e:
        print(f"[{pid}] ERROR: {e}", flush=True)


async def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--db-uri", required=True)
    args = parser.parse_args()

    conn = psycopg2.connect(args.db_uri)
    cur = conn.cursor()
    cur.execute("""
        SELECT pid, friends_nex_password, last_auth_host, last_auth_port
        FROM nex_accounts
        WHERE friends_nex_password IS NOT NULL
          AND last_auth_host IS NOT NULL
          AND last_auth_port IS NOT NULL
    """)
    users = cur.fetchall()
    cur.close()
    conn.close()

    if not users:
        print("No users with stored auth host yet. Connect each Wii U once so fetch_friends.py "
              "can record the auth server address, then re-run this script.", file=sys.stderr)
        sys.exit(1)

    print(f"Seeding preferences for {len(users)} user(s)...", flush=True)
    for pid, password, auth_host, auth_port in users:
        await seed_user(int(pid), password, auth_host, int(auth_port), args.db_uri)
        await asyncio.sleep(1)  # be polite to Pretendo's server


asyncio.run(main())
