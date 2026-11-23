#!/usr/bin/env python3
"""
Wii Karaoke U by JOYSOUND - NEX game server (rankings / unique ids).

  Game server ID : 0x10100D01
  Access key     : dfc5a4ac
  NEX version    : 3.2.1  (from the DDL parse trees in app.rpx)

Runs the authentication server (TicketGranting) and the secure server
(SecureConnection, Utility, Ranking) on two UDP ports.

  python3 nex_server.py            # reads nex_config.json
"""
import asyncio
import json
import logging
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from wahpnex import passwords
from wahpnex.prudp import PRUDPServer
from wahpnex.protocols import NAMES, Auth, Secure
from wahpnex.ranking_store import RankingStore
from wahpnex.rmc import Dispatcher

DEFAULTS = {
    "access_key": "dfc5a4ac",
    "game_server_id": "10100D01",
    "bind": "0.0.0.0",
    "public_ip": "192.168.1.10",       # address the console uses to reach this machine
    "auth_port": 60000,
    "secure_port": 60001,
    "secure_server_password": "change-this-secure-server-secret",
    "build_name": "branch:origin/release/ngs/3.2.x build:3_2_1_2_0",
    "password_provider": "json",       # json | pretendo | static
    "password_file": "nex_passwords.json",
    "pretendo_grpc": {"host": "127.0.0.1", "port": 50051, "api_key": ""},
    "static_password": "",
    "database": "nex_rankings.sqlite3",
    # what the game's custom time scopes 0 and 1 mean: "all", "day", "week", "month" or seconds
    "time_scopes": {"0": "all", "1": "all"},
    "log_level": "INFO",
    # Pretendo's account server UDP-pings `health_check_port` and expects the
    # same bytes back before it hands out this server. 0 = disabled.
    "health_check_port": 0,
}


class Echo(asyncio.DatagramProtocol):
    def connection_made(self, t): self.t = t
    def datagram_received(self, data, addr): self.t.sendto(data, addr)


def load_config(path):
    cfg = dict(DEFAULTS)
    if os.path.exists(path):
        with open(path, "r", encoding="utf-8") as f:
            cfg.update(json.load(f))
    # Secrets come from the environment (.env, sourced by start.sh), never from the repo.
    if os.environ.get("WAHP_KERBEROS_PASSWORD"):
        cfg["secure_server_password"] = os.environ["WAHP_KERBEROS_PASSWORD"]
    if os.environ.get("PN_WUC_ACCOUNT_GRPC_API_KEY"):
        cfg["pretendo_grpc"]["api_key"] = os.environ["PN_WUC_ACCOUNT_GRPC_API_KEY"]
    return cfg


async def main(cfg):
    here = os.path.dirname(os.path.abspath(__file__))
    db = cfg["database"] if os.path.isabs(cfg["database"]) else os.path.join(here, cfg["database"])
    scopes = {int(k): v for k, v in cfg["time_scopes"].items()}
    scopes[2] = "all"
    store = RankingStore(db, scopes)
    auth = Auth(cfg, passwords.make(cfg))
    secure = Secure(cfg, store)

    loop = asyncio.get_running_loop()
    auth_srv = PRUDPServer("auth", cfg["access_key"], Dispatcher("auth", auth.table(), NAMES))
    sec_srv = PRUDPServer("secure", cfg["access_key"], Dispatcher("secure", secure.table(), NAMES),
                          login_handler=auth.secure_login)
    await loop.create_datagram_endpoint(lambda: auth_srv, local_addr=(cfg["bind"], cfg["auth_port"]))
    await loop.create_datagram_endpoint(lambda: sec_srv, local_addr=(cfg["bind"], cfg["secure_port"]))
    if cfg["health_check_port"]:
        await loop.create_datagram_endpoint(Echo, local_addr=(cfg["bind"], cfg["health_check_port"]))
        logging.info("health check echo udp/%d", cfg["health_check_port"])
    logging.info("NEX auth server   udp/%d", cfg["auth_port"])
    logging.info("NEX secure server udp/%d  (advertised as %s)", cfg["secure_port"], auth.secure_url())
    await asyncio.Event().wait()


if __name__ == "__main__":
    cfg = load_config(sys.argv[1] if len(sys.argv) > 1 else os.environ.get("NEX_CONFIG", "nex_config.json"))
    logging.basicConfig(level=getattr(logging, cfg["log_level"]), format="%(asctime)s %(name)s %(levelname)s %(message)s")
    try:
        asyncio.run(main(cfg))
    except KeyboardInterrupt:
        pass
