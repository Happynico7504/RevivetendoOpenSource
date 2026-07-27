#!/usr/bin/env python3
"""
Connect to Pretendo's Friends NEX auth server as a PRUDP client, fetch the
full friend list via UpdateAndGetAllInformation, and upsert into pretendo_friends.
"""

import argparse
import asyncio
import json
import os
import sys

import psycopg2
from nintendo.nex import backend, settings as nex_settings, friends as friends_lib
from nintendo.nex import common, rmc as _rmc_module
from nintendo.nex.nintendonotification import NintendoNotificationServer

# Pretendo's nex-go v2 server uses UseStructureHeader=false (no version byte or
# length prefix in structs). NintendoClients auto-sets nex.struct_header=True when
# PRUDP minor_version>=3, which mismatches. Force it back to False.
_orig_rmc_init = _rmc_module.RMCClient.__init__
def _patched_rmc_init(self, settings, client):
    _orig_rmc_init(self, settings, client)
    self.settings["nex.struct_header"] = False
_rmc_module.RMCClient.__init__ = _patched_rmc_init


NOTIFY_TEST_BIN = "/nico-pretendo-bridge/friends-nex/notify-test"


class NotificationForwarder(NintendoNotificationServer):
    """Receives Pretendo push notifications and forwards them to the local Wii U via notify-test."""

    def __init__(self, owner_pid: int):
        super().__init__()
        self.owner_pid = owner_pid

    async def _forward(self, sender_pid: int, mode: str):
        try:
            proc = await asyncio.create_subprocess_exec(
                NOTIFY_TEST_BIN, str(self.owner_pid), str(sender_pid), mode,
                stdout=asyncio.subprocess.PIPE, stderr=asyncio.subprocess.PIPE,
            )
            stdout, stderr = await asyncio.wait_for(proc.communicate(), timeout=5)
            print(f"  [notif] forwarded {mode} from PID={sender_pid}: {stdout.decode().strip()}", flush=True)
        except Exception as e:
            print(f"  [notif] forward {mode} from PID={sender_pid} failed: {e}", flush=True)

    async def _handle_event(self, event):
        sender_pid = int(event.pid)
        notif_type = int(event.type)
        if notif_type == 24:    # PRESENCE_CHANGE — friend came online / started title
            await self._forward(sender_pid, "online")
        elif notif_type == 10:  # LOGOUT — friend went offline
            await self._forward(sender_pid, "offline")
        else:
            print(f"  [notif] ignoring type={notif_type} from PID={sender_pid}", flush=True)

    async def process_nintendo_notification_event(self, client, event):
        await self._handle_event(event)

    async def process_nintendo_notification_event_alt(self, client, event):
        await self._handle_event(event)


ACCESS_KEY = "ridfebb9"
NEX_VERSION = 31011  # WiiU Friends NEX 3.10.11


def _persist_auth_host(db_uri: str, pid: int, host: str, port: int):
    try:
        conn = psycopg2.connect(db_uri)
        cur = conn.cursor()
        cur.execute("ALTER TABLE nex_accounts ADD COLUMN IF NOT EXISTS last_auth_host TEXT")
        cur.execute("ALTER TABLE nex_accounts ADD COLUMN IF NOT EXISTS last_auth_port INT")
        cur.execute(
            "UPDATE nex_accounts SET last_auth_host = %s, last_auth_port = %s WHERE pid = %s",
            (host, port, pid),
        )
        conn.commit()
        cur.close()
        conn.close()
    except Exception as e:
        print(f"  warning: could not persist auth host: {e}", flush=True)


async def fetch_friends(pid: int, nex_password: str, auth_host: str, auth_port: int, db_uri: str, keep_alive: bool = False):
    s = nex_settings.default()
    s["prudp.access_key"] = ACCESS_KEY
    s["nex.version"] = NEX_VERSION
    s["kerberos.key_size"] = 16

    username = str(pid)
    print(f"Connecting to {auth_host}:{auth_port} as PID={pid}", flush=True)

    # Persist the auth host so seed_preferences.py can re-use it for other users.
    _persist_auth_host(db_uri, pid, auth_host, auth_port)

    # Load the locally stored NNAInfo and presence (saved by the Wii U on connect).
    local_nnid = ""
    local_mii_name = ""
    local_mii_data = b""
    local_is_online = False
    local_title_id = 0
    local_title_version = 0
    local_game_server_id = 0
    try:
        conn_pre = psycopg2.connect(db_uri)
        cur_pre = conn_pre.cursor()
        cur_pre.execute(
            "SELECT nnid, mii_name, mii_data, is_online, "
            "presence_title_id, presence_title_version, presence_game_server_id "
            "FROM user_settings WHERE pid = %s", (pid,)
        )
        row = cur_pre.fetchone()
        if row:
            local_nnid        = row[0] or ""
            local_mii_name    = row[1] or ""
            local_mii_data    = bytes(row[2]) if row[2] else b""
            local_is_online   = bool(row[3])
            local_title_id    = int(row[4]) if row[4] else 0
            local_title_version = int(row[5]) if row[5] else 0
            local_game_server_id = int(row[6]) if row[6] else 0
        cur_pre.close()
        conn_pre.close()
    except Exception as e:
        print(f"  warning: could not load local NNA/presence: {e}", flush=True)

    async with backend.connect(s, auth_host, auth_port) as be:
        async with be.login(username, nex_password) as client:
            friends_client = friends_lib.FriendsClientV2(client)

            # Build NNAInfo using data received from the Wii U (so Pretendo sees our real Mii).
            nna_info = friends_lib.NNAInfo()
            nna_info.principal_info = friends_lib.PrincipalBasicInfo()
            nna_info.principal_info.pid = pid
            nna_info.principal_info.nnid = local_nnid
            nna_info.principal_info.mii = friends_lib.MiiV2()
            nna_info.principal_info.mii.name = local_mii_name
            nna_info.principal_info.mii.unk1 = 0
            nna_info.principal_info.mii.unk2 = 0
            nna_info.principal_info.mii.data = local_mii_data
            nna_info.principal_info.mii.datetime = common.DateTime(0)
            nna_info.principal_info.unk = 2
            nna_info.unk1 = 94
            nna_info.unk2 = 11

            presence = friends_lib.NintendoPresenceV2()
            presence.flags = 0x1 | 0x4 | 0x10  # GameKey | JoinAvailability | GameServerID
            presence.is_online = local_is_online
            presence.game_key = friends_lib.GameKey()
            presence.game_key.title_id = local_title_id
            presence.game_key.title_version = local_title_version
            presence.unk1 = 0
            presence.message = ""
            presence.unk2 = 0
            presence.unk3 = 0
            presence.game_server_id = local_game_server_id
            presence.unk4 = 0
            presence.pid = pid
            presence.gathering_id = 0
            presence.application_data = b""
            presence.unk5 = 3
            presence.unk6 = 3
            presence.unk7 = 3

            birthday = common.DateTime(0)

            response = await friends_client.update_and_get_all_information(
                nna_info, presence, birthday
            )

            friend_list = response.friends
            received_requests = response.received_requests
            pretendo_pref = response.principal_preference
            pretendo_comment = response.comment
            print(f"Got {len(friend_list)} friends, {len(received_requests)} incoming requests for PID={pid}", flush=True)

            conn = psycopg2.connect(db_uri)
            cur = conn.cursor()

            # 2-way preference/comment sync.
            # If we have local settings → push them to Pretendo (Wii U is source of truth).
            # If no local settings yet → seed our DB from Pretendo.
            cur.execute("SELECT show_online_presence, show_current_title, block_friend_requests, "
                        "comment_unknown, comment_text FROM user_settings WHERE pid = %s", (pid,))
            local = cur.fetchone()
            if local:
                local_pref = friends_lib.PrincipalPreference()
                local_pref.show_online_status = local[0]
                local_pref.show_current_title = local[1]
                local_pref.block_friend_requests = local[2]
                await friends_client.update_preference(local_pref)
                local_comment = friends_lib.Comment()
                local_comment.unk = local[3]
                local_comment.text = local[4]
                local_comment.changed = common.DateTime(0)
                await friends_client.update_comment(local_comment)
                print(f"  pushed local preference/comment to Pretendo", flush=True)
            else:
                # Seed from Pretendo
                changed_ts = pretendo_comment.changed.standard_datetime() if (pretendo_comment.changed and pretendo_comment.changed.value() != 0) else None
                cur.execute("""
                    INSERT INTO user_settings
                        (pid, show_online_presence, show_current_title, block_friend_requests,
                         comment_unknown, comment_text, comment_changed_at)
                    VALUES (%s, %s, %s, %s, %s, %s, COALESCE(%s, NOW()))
                    ON CONFLICT (pid) DO NOTHING
                """, (
                    pid,
                    bool(pretendo_pref.show_online_status),
                    bool(pretendo_pref.show_current_title),
                    bool(pretendo_pref.block_friend_requests),
                    int(pretendo_comment.unk or 0),
                    pretendo_comment.text or "",
                    changed_ts,
                ))
                print(f"  seeded preference/comment from Pretendo", flush=True)

            for fi in friend_list:
                try:
                    friend_pid = fi.nna_info.principal_info.pid
                    friend_nnid = fi.nna_info.principal_info.nnid or ""
                    mii_name = fi.nna_info.principal_info.mii.name or ""
                    mii_data = fi.nna_info.principal_info.mii.data or b""
                    is_online = bool(fi.presence.is_online)
                    game_server_id = fi.presence.game_server_id or 0
                    title_id = int(fi.presence.game_key.title_id) if fi.presence.game_key else 0
                    title_version = int(fi.presence.game_key.title_version) if fi.presence.game_key else 0
                    presence_flags = int(fi.presence.flags) if fi.presence.flags else 0
                    presence_pid = int(fi.presence.pid) if fi.presence.pid else 0
                    presence_gathering_id = int(fi.presence.gathering_id) if fi.presence.gathering_id else 0
                    presence_unk5 = int(fi.presence.unk5) if fi.presence.unk5 else 3
                    presence_unk6 = int(fi.presence.unk6) if fi.presence.unk6 else 3
                    presence_unk7 = int(fi.presence.unk7) if fi.presence.unk7 else 3
                    # befriended/last_online are DateTime values (unix-like encoding)
                    befriended_ts = fi.befriended.timestamp() if fi.befriended and fi.befriended.value else None
                    last_online_ts = fi.last_online.timestamp() if fi.last_online and fi.last_online.value else None

                    cur.execute("""
                        INSERT INTO pretendo_friends
                            (owner_pid, friend_pid, friend_nnid, mii_name, mii_data,
                             is_online, game_server_id, title_id, title_version,
                             presence_flags, presence_pid, presence_gathering_id,
                             presence_unk5, presence_unk6, presence_unk7,
                             befriended_at, last_online, updated_at)
                        VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s,
                            to_timestamp(%s), to_timestamp(%s), NOW())
                        ON CONFLICT (owner_pid, friend_pid) DO UPDATE SET
                            friend_nnid           = EXCLUDED.friend_nnid,
                            mii_name              = EXCLUDED.mii_name,
                            mii_data              = EXCLUDED.mii_data,
                            is_online             = EXCLUDED.is_online,
                            game_server_id        = EXCLUDED.game_server_id,
                            title_id              = EXCLUDED.title_id,
                            title_version         = EXCLUDED.title_version,
                            presence_flags        = EXCLUDED.presence_flags,
                            presence_pid          = EXCLUDED.presence_pid,
                            presence_gathering_id = EXCLUDED.presence_gathering_id,
                            presence_unk5         = EXCLUDED.presence_unk5,
                            presence_unk6         = EXCLUDED.presence_unk6,
                            presence_unk7         = EXCLUDED.presence_unk7,
                            befriended_at  = COALESCE(pretendo_friends.befriended_at, EXCLUDED.befriended_at),
                            last_online    = EXCLUDED.last_online,
                            updated_at     = NOW()
                    """, (
                        pid, friend_pid, friend_nnid, mii_name,
                        psycopg2.Binary(mii_data),
                        is_online, game_server_id, title_id, title_version,
                        presence_flags, presence_pid, presence_gathering_id,
                        presence_unk5, presence_unk6, presence_unk7,
                        befriended_ts, last_online_ts,
                    ))

                    # Cache the friend's PNID so gRPC lookups are fast.
                    if friend_nnid:
                        cur.execute("""
                            INSERT INTO pnid_cache (pid, pnid)
                            VALUES (%s, %s)
                            ON CONFLICT (pid) DO UPDATE SET pnid = EXCLUDED.pnid, updated_at = NOW()
                        """, (friend_pid, friend_nnid))

                    # Sync friend's status comment into user_settings.
                    # Only overwrite if Pretendo's timestamp is newer than what we have locally
                    # (preserves comments set directly on our server).
                    comment = fi.comment
                    comment_text = comment.text or ""
                    comment_unknown = int(comment.unk or 0)
                    comment_changed_ts = None
                    if comment.changed and comment.changed.value() != 0:
                        try:
                            comment_changed_ts = comment.changed.standard_datetime()
                        except Exception:
                            pass
                    cur.execute("""
                        INSERT INTO user_settings
                            (pid, comment_unknown, comment_text, comment_changed_at)
                        VALUES (%s, %s, %s, COALESCE(%s::timestamptz, NOW()))
                        ON CONFLICT (pid) DO UPDATE SET
                            comment_unknown    = EXCLUDED.comment_unknown,
                            comment_text       = EXCLUDED.comment_text,
                            comment_changed_at = EXCLUDED.comment_changed_at
                        WHERE user_settings.comment_text = ''
                           OR user_settings.comment_changed_at < EXCLUDED.comment_changed_at
                    """, (friend_pid, comment_unknown, comment_text, comment_changed_ts))

                    print(f"  stored friend PID={friend_pid} NNID={friend_nnid} online={is_online} comment={comment_text!r}", flush=True)
                except Exception as e:
                    print(f"  error storing friend PID={getattr(fi.nna_info.principal_info, 'pid', '?')}: {e}", flush=True)

            # Store incoming (received) friend requests so the Wii U sees them on next connect.
            seen_requester_pids = set()
            for req in received_requests:
                try:
                    req_pid   = int(req.principal_info.pid)
                    req_nnid  = req.principal_info.nnid or ""
                    req_mname = req.principal_info.mii.name if req.principal_info.mii else ""
                    req_mdata = bytes(req.principal_info.mii.data) if (req.principal_info.mii and req.principal_info.mii.data) else b""
                    msg       = req.message
                    req_id    = int(msg.friend_request_id) if msg.friend_request_id else 0
                    req_msg   = msg.message or ""
                    req_unk1  = int(msg.unk1) if msg.unk1 is not None else 0
                    req_unk2  = int(msg.unk2) if msg.unk2 is not None else 0
                    req_unk3  = int(msg.unk3) if msg.unk3 is not None else 0
                    req_str   = msg.string or ""
                    req_tid   = int(msg.game_key.title_id) if (msg.game_key and msg.game_key.title_id) else 0
                    req_tver  = int(msg.game_key.title_version) if (msg.game_key and msg.game_key.title_version) else 0
                    req_sent  = msg.datetime.standard_datetime() if (msg.datetime and msg.datetime.value()) else None
                    req_exp   = msg.expires.standard_datetime() if (msg.expires and msg.expires.value()) else None

                    cur.execute("""
                        INSERT INTO pretendo_friend_requests
                            (id, owner_pid, requester_pid, nnid, mii_name, mii_data,
                             message, unk1, unk2, unk3, str_field,
                             title_id, title_version, sent_on, expires_on)
                        VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s)
                        ON CONFLICT (owner_pid, requester_pid) DO UPDATE SET
                            id          = EXCLUDED.id,
                            nnid        = EXCLUDED.nnid,
                            mii_name    = EXCLUDED.mii_name,
                            mii_data    = EXCLUDED.mii_data,
                            message     = EXCLUDED.message,
                            unk1        = EXCLUDED.unk1,
                            unk2        = EXCLUDED.unk2,
                            unk3        = EXCLUDED.unk3,
                            str_field   = EXCLUDED.str_field,
                            title_id    = EXCLUDED.title_id,
                            title_version = EXCLUDED.title_version,
                            sent_on     = EXCLUDED.sent_on,
                            expires_on  = EXCLUDED.expires_on
                    """, (req_id, pid, req_pid, req_nnid, req_mname,
                          psycopg2.Binary(req_mdata),
                          req_msg, req_unk1, req_unk2, req_unk3, req_str,
                          req_tid, req_tver, req_sent, req_exp))
                    seen_requester_pids.add(req_pid)
                    print(f"  stored incoming request from PID={req_pid} NNID={req_nnid}", flush=True)
                except Exception as e:
                    print(f"  error storing incoming request: {e}", flush=True)

            # Remove requests that are no longer pending on Pretendo (accepted/declined there).
            if seen_requester_pids:
                cur.execute(
                    "DELETE FROM pretendo_friend_requests WHERE owner_pid = %s AND requester_pid != ALL(%s)",
                    (pid, list(seen_requester_pids))
                )
            else:
                cur.execute("DELETE FROM pretendo_friend_requests WHERE owner_pid = %s", (pid,))

            conn.commit()

            # Process pending commands queued by the Wii U client (add/remove friends).
            try:
                import json as _json
                cur.execute(
                    "SELECT id, method, args_json FROM pending_pretendo_commands WHERE pid = %s ORDER BY id",
                    (pid,)
                )
                commands = cur.fetchall()
                processed_ids = []
                for cmd_id, method, args_json in commands:
                    try:
                        args = _json.loads(args_json) if args_json else {}
                        if method == "add_friend":
                            gk = friends_lib.GameKey()
                            gk.title_id = 0
                            gk.title_version = 0
                            await friends_client.add_friend_request(
                                int(args["target_pid"]), 0, "", 0, "", gk, common.DateTime(0)
                            )
                            print(f"  forwarded add_friend target_pid={args['target_pid']}", flush=True)
                        elif method == "accept_friend_request":
                            await friends_client.accept_friend_request(int(args["request_id"]))
                            print(f"  forwarded accept_friend_request request_id={args['request_id']}", flush=True)
                        elif method == "remove_friend":
                            await friends_client.remove_friend(args["target_pid"])
                            print(f"  forwarded remove_friend target_pid={args['target_pid']}", flush=True)
                        else:
                            print(f"  unknown pending command: {method}", flush=True)
                        processed_ids.append(cmd_id)
                    except Exception as e:
                        print(f"  error processing command {method}: {e}", flush=True)
                if processed_ids:
                    cur.execute(
                        "DELETE FROM pending_pretendo_commands WHERE id = ANY(%s)",
                        (processed_ids,)
                    )
                    conn.commit()
            except Exception as e:
                print(f"  warning: could not process pending commands: {e}", flush=True)

            cur.close()
            conn.close()

            if keep_alive:
                print(f"  keep-alive PID={pid}: holding Pretendo connection open", flush=True)
                client.register_server(NotificationForwarder(pid))
                socket_path = f"/tmp/pretendo-presence-{pid}.sock"
                try:
                    os.unlink(socket_path)
                except FileNotFoundError:
                    pass

                async def _handle_cmd(reader, writer):
                    try:
                        raw = await reader.readline()
                        cmd = json.loads(raw)
                        c = cmd.get("cmd", "")
                        if c == "update_presence":
                            p = friends_lib.NintendoPresenceV2()
                            p.flags = 0x1 | 0x4 | 0x10
                            p.is_online = bool(cmd.get("is_online", False))
                            p.game_key = friends_lib.GameKey()
                            p.game_key.title_id = int(cmd.get("title_id", 0))
                            p.game_key.title_version = int(cmd.get("title_version", 0))
                            p.game_server_id = int(cmd.get("game_server_id", 0))
                            p.unk1 = 0
                            p.message = ""
                            p.unk2 = 0
                            p.unk3 = 0
                            p.pid = pid
                            p.gathering_id = 0
                            p.application_data = b""
                            p.unk4 = 0
                            p.unk5 = 3
                            p.unk6 = 3
                            p.unk7 = 3
                            await friends_client.update_presence(p)
                        elif c == "update_mii":
                            m = friends_lib.MiiV2()
                            m.name = cmd.get("name", "")
                            m.unk1 = 0
                            m.unk2 = 0
                            m.data = bytes.fromhex(cmd.get("data_hex", ""))
                            m.datetime = common.DateTime(0)
                            await friends_client.update_mii(m)
                        elif c == "add_friend":
                            gk = friends_lib.GameKey()
                            gk.title_id = 0
                            gk.title_version = 0
                            await friends_client.add_friend_request(
                                int(cmd["target_pid"]), 0, "", 0, "", gk, common.DateTime(0)
                            )
                        elif c == "accept_friend_request":
                            await friends_client.accept_friend_request(int(cmd["request_id"]))
                        elif c == "remove_friend":
                            await friends_client.remove_friend(int(cmd["target_pid"]))
                        elif c == "update_comment":
                            co = friends_lib.Comment()
                            co.unk = int(cmd.get("unk", 0))
                            co.text = cmd.get("text", "")
                            co.changed = common.DateTime(0)
                            await friends_client.update_comment(co)
                        elif c == "update_preference":
                            pref = friends_lib.PrincipalPreference()
                            pref.show_online_status = bool(cmd.get("show_online", True))
                            pref.show_current_title = bool(cmd.get("show_title", True))
                            pref.block_friend_requests = bool(cmd.get("block_requests", False))
                            await friends_client.update_preference(pref)
                        else:
                            raise ValueError(f"unknown command: {c}")
                        writer.write(b'{"ok":true}\n')
                    except Exception as e:
                        err = str(e).replace('"', "'")
                        writer.write(f'{{"ok":false,"error":"{err}"}}\n'.encode())
                    await writer.drain()
                    writer.close()

                server = await asyncio.start_unix_server(_handle_cmd, socket_path)
                try:
                    await asyncio.sleep(86400)  # 24 h; PRUDP pings run in the background
                finally:
                    server.close()
                    try:
                        os.unlink(socket_path)
                    except FileNotFoundError:
                        pass


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--pid", type=int, required=True)
    parser.add_argument("--nex-password", required=True)
    parser.add_argument("--auth-host", required=True)
    parser.add_argument("--auth-port", type=int, required=True)
    parser.add_argument("--db-uri", required=True)
    parser.add_argument("--keep-alive", action="store_true",
                        help="Hold the Pretendo connection open after sync so this user appears online")
    args = parser.parse_args()

    asyncio.run(fetch_friends_loop(
        args.pid, args.nex_password,
        args.auth_host, args.auth_port,
        args.db_uri, args.keep_alive,
    ))


async def fetch_friends_loop(pid, nex_password, auth_host, auth_port, db_uri, keep_alive):
    while True:
        try:
            await fetch_friends(pid, nex_password, auth_host, auth_port, db_uri, keep_alive)
        except Exception as e:
            if not keep_alive:
                raise
            print(f"  keep-alive PID={pid}: connection lost ({e}), reconnecting in 5s", flush=True)
            await asyncio.sleep(5)
            continue
        if not keep_alive:
            break
        # keep-alive: loop to reconnect after 24 h or clean exit


if __name__ == "__main__":
    main()
