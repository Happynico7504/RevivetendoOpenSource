#!/usr/bin/env python3
"""
Wii Karaoke U by JOYSOUND (EU) - replacement game server.

  python3 server.py              (reads config.json if present)

Point the game at it with tools/rpx_patch_url.py (recommended) or a DNS
redirect of ssl.wahp.wah.wup.app.nintendo.net plus an SSL patch.
"""
import hmac
import json
import logging
import os
import sys
import time

from flask import Flask, Response, abort, request, send_from_directory

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
from wahp import config as config_mod
from wahp.content import Content
from wahp.handlers import Handlers, private_link_signature
from wahp.protocol import decode_params
from wahp.shop import make_shop
from wahp.tickets import Store
from wahp.identity import client_ip, game_account_key
from wahp.protocol import Reply

log = logging.getLogger("wahp")


def create_app(cfg=None):
    cfg = cfg or config_mod.load()
    here = os.path.dirname(os.path.abspath(__file__))
    content_dir = cfg["content_dir"] if os.path.isabs(cfg["content_dir"]) else os.path.join(here, cfg["content_dir"])
    private_dir = cfg["private_dir"] if os.path.isabs(cfg["private_dir"]) else os.path.join(here, cfg["private_dir"])
    cfg["content_dir"] = content_dir
    cfg["private_dir"] = private_dir
    db_path = cfg["database"] if os.path.isabs(cfg["database"]) else os.path.join(here, cfg["database"])

    app = Flask(__name__)
    store = Store(db_path, cfg)
    content = Content(content_dir, cfg["public_url"])
    handlers = Handlers(cfg, store, content)
    app.config["WAHP"] = cfg

    @app.route(cfg["endpoint"], methods=["GET", "POST"])
    def game():
        src = request.form if request.method == "POST" else request.args
        params = decode_params(src)           # first value wins (MA0001 sends its own cid/lid first)
        typ = (params.get("type") or "").strip()
        key, note = game_account_key(request, cfg, params)
        if key is None:
            log.warning("rejected %s from %s: %s", typ, request.remote_addr, note)
            # err category 2 = ticket/auth error: the game clears its ticket state and shows the message
            return Response(Reply().error_message(2, note).bytes(), mimetype="application/octet-stream")
        store.account(key, params.get("cid"), params.get("lid"), client_ip(request, cfg))
        shown = {k: (v[:12] + "..." if k == "atk" and v and len(v) > 15 else v) for k, v in params.items()}
        if cfg["log_requests"]:
            log.info("%s %s %s %s", request.method, key, typ, json.dumps(shown, ensure_ascii=False))
        if typ.startswith("LG"):
            store.log(key, typ, json.dumps(shown, ensure_ascii=False))
        body = handlers.handle(typ, params, key)
        return Response(body, mimetype="application/octet-stream")

    @app.route("/content/private/<int:expires>/<sig>/<name>")
    def private_file(expires, sig, name):
        good = hmac.compare_digest(sig, private_link_signature(cfg, name, expires))
        if not good or expires < time.time() or cfg["admin_key"] == "change-me":
            abort(404)
        return send_from_directory(private_dir, name, conditional=True)

    @app.route("/content/community-songs/<name>")
    def community_song_file(name):
        pid = request.args.get("pid", "")
        if not pid.isdigit() or not store.known_pid(pid):
            abort(404)
        return send_from_directory(os.path.join(content_dir, "community-songs"), name, conditional=True)

    @app.route("/content/<path:rel>")
    def content_file(rel):
        # Flask's send_from_directory honours Range requests (the game asks for
        # "%u-%u" byte ranges).
        return send_from_directory(content_dir, rel, conditional=True)

    app.register_blueprint(make_shop(cfg, store))
    from wahp.rankings import make_rankings
    app.register_blueprint(make_rankings(cfg, here))
    return app


def rebuild_karaoke_nand_if_stale(cfg, here):
    """content/karaoke_nand/ (the on-NAND form of the catalog) is a derived
    build artifact of content/db/main.bin, not something a song-deploy step
    should have to remember to regenerate by hand - that's exactly how a
    stale/invalid NAND catalog slipped through before. Rebuild it
    automatically on every server start whenever main.bin has moved on."""
    content_dir = cfg["content_dir"] if os.path.isabs(cfg["content_dir"]) else os.path.join(here, cfg["content_dir"])
    catalog_path = os.path.join(content_dir, "db", "main.bin")
    out_dir = os.path.join(content_dir, "karaoke_nand")
    if not os.path.exists(catalog_path):
        return
    sys.path.insert(0, os.path.join(here, "tools"))
    from build_karaoke_nand import build, needs_rebuild, DEFAULT_RPX
    if not needs_rebuild(catalog_path, out_dir):
        return
    header_template = os.path.join(here, "tools", "nand_header_template.bin")
    rpx = cfg.get("karaoke_nand_rpx", DEFAULT_RPX)
    log.info("content/db/main.bin changed - rebuilding content/karaoke_nand/")
    version, written = build(catalog_path, header_template, out_dir, rpx)
    for path, size in written:
        log.info("wrote %s%s", path, f" ({size} bytes)" if size is not None else "")
    log.info("karaoke_nand rebuilt, version %s", version)


if __name__ == "__main__":
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
    cfg = config_mod.load()
    rebuild_karaoke_nand_if_stale(cfg, os.path.dirname(os.path.abspath(__file__)))
    app = create_app(cfg)
    ssl = None
    if cfg.get("ssl_cert") and cfg.get("ssl_key"):
        ssl = (cfg["ssl_cert"], cfg["ssl_key"])
    app.run(host=cfg["host"], port=cfg["port"], ssl_context=ssl, threaded=True)
