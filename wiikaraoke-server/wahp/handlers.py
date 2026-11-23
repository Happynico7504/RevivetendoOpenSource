"""
One handler per 'type' code. Formats follow the RecvData parsers in app.rpx:

 type    game command (class)            request params        reply body after header
 ------  ------------------------------  --------------------  ---------------------------------------
 SA0001  cGameNetCmdSendToken            -                     line token_expiry(16), line server_now(16)
 TE0001  cGameNetCmdGetPurchaseHistory   -                     u32 purchase count
 BA0001  cGameNetCmdGetServicePeriod     -                     u8 status, line expiry(16), line now(16), u8 permanent
 SD0001  cGameNetCmdGetMusicURL          sid(%07u), rlog       u32 size, line url(512)
 WO0001  cGameNetCmdGetLyrics            sid(%u)               u32 size, line url(512)
 MD0001  cGameNetCmdGetMovieURL          mid, mno(%02d)        u32 size, line url(512)
 GD0001  cGameNetCmdGetDBURL             -                     line id(32), u32 size, line url(512)
 GM0001  cGameNetCmdGetMovieDBURL        -                     line id(32), u32 size, line url(512)
 GL0001  cGameNetCmdGetDBPatchList       mdid                  u16 count, count x line patch_id(32)
 GP0001  cGameNetCmdGetDBPatchURL        pdid                  u32 size, line url(512)
 GF0001  cGameNetCmdGetFirstLineDBURL    dbid                  u32 size, line url(512)
 FE0001  cGameNetCmdGetFeatureURL        -                     line id(32), u32 size, line url(512)
 MA0001  cGameNetCmdGetMagazineURL       cid, lid(%02d)        line id(32), u32 size, line url(512)
 NI0002  cGameNetCmdGetUrgentInfoURL     -                     u8 has(0/1) [, u32 size, line url(512)]
 LG0002..LG0005 (POST) cGameNetCmdSendLog  log fields          (header only)

Header: 00 01 = OK, 00 02 = no record (game error 20).
"""
import hashlib
import hmac
import os
import time
import urllib.parse
from datetime import timedelta
from .protocol import Reply, game_time


def private_link_signature(cfg, name, expires):
    msg = "%s|%d" % (name, expires)
    return hmac.new(cfg["admin_key"].encode(), msg.encode(), hashlib.sha256).hexdigest()


def _url_reply(item):
    if item is None:
        return Reply().no_record().bytes()
    return Reply().ok().u32(item["size"]).line(item["url"], 512).bytes()


def _id_url_reply(item):
    if item is None:
        return Reply().no_record().bytes()
    return Reply().ok().line(item["id"], 32).u32(item["size"]).line(item["url"], 512).bytes()


# The id of the magazine/feature files bundled with the game (hardcoded at 0x0222BE94).
# We have no downloadable versions of these, and answering "no record" makes the game show
# error 140-1632 (error 20), so report what the console already has, with nothing to download.
BUNDLED_ID = "2013061200_00"


class Handlers:
    def __init__(self, cfg, store, content):
        self.cfg, self.store, self.content = cfg, store, content
        self.table = {
            "SA0001": self.send_token,
            "TE0001": self.purchase_history,
            "BA0001": self.service_period,
            "SD0001": self.music_url,
            "WO0001": self.lyrics_url,
            "MD0001": self.movie_url,
            "GD0001": self.db_url,
            "GM0001": self.movie_db_url,
            "GL0001": self.db_patch_list,
            "GP0001": self.db_patch_url,
            "GF0001": self.first_line_db_url,
            "FE0001": self.feature_url,
            "MA0001": self.magazine_url,
            "NI0002": self.urgent_info,
            "LG0002": self.send_log, "LG0003": self.send_log,
            "LG0004": self.send_log, "LG0005": self.send_log,
        }

    def handle(self, typ, p, account):
        fn = self.table.get(typ)
        if fn is None:
            return Reply().fail(3).bytes()
        return fn(p, account)

    # --- session / tickets -------------------------------------------------
    def send_token(self, p, account):
        # 0x02117c50: requires server_now <= token_expiry, then stores the
        # server clock offset (0x0210c870). The token is re-sent ~1h before expiry.
        _, _, now, _ = self.store.service_period(account)
        exp = now + timedelta(hours=self.cfg["token_lifetime_hours"])
        return Reply().ok().line(game_time(exp), 16).line(game_time(now), 16).bytes()

    def purchase_history(self, p, account):
        return Reply().ok().u32(len(self.store.purchases(account))).bytes()

    def service_period(self, p, account):
        status, expiry, now, permanent = self.store.service_period(account)
        return (Reply().ok().u8(status)
                .line(game_time(expiry), 16).line(game_time(now), 16)
                .u8(permanent).bytes())

    # --- content URLs ------------------------------------------------------
    def _private_song(self, sid, account):
        """The player must be identified by a verified PID: an IP says nothing about who it is."""
        return self._private_item(self.cfg["private_songs"].get(sid or ""), account)

    def _custom_song(self, sid, account):
        """Any custom song, served to any account with a verified PID (not
        just an IP that happens to know/guess the URL) - no per-song config
        entry needed, unlike _private_song's explicit pids-list whitelist
        (left untouched for whatever still wants that tighter restriction).
        File is found purely by naming convention: content/community-songs/
        <sid>.ujk. The URL carries the caller's own PID as a plain query
        param, checked against the accounts table on every request (see
        Store.known_pid) - simpler than _private_item's signed/expiring
        link since community songs are meant to be freely, repeatedly
        playable by any PID this server has ever seen, not a one-time
        download; the PID itself is the gate, not a signature."""
        if not sid or not sid.isdigit() or not account.startswith("pid:"):
            return None
        pid = account[4:]
        filename = "%s.ujk" % sid
        path = os.path.join(self.cfg["content_dir"], "community-songs", filename)
        if not os.path.isfile(path):
            return None
        url = "%s/content/community-songs/%s?pid=%s" % (self.content.public_url, urllib.parse.quote(filename), pid)
        return {"url": url, "size": os.path.getsize(path), "id": self.content.md5(path)[:32]}

    def _private_item(self, song, account):
        if song is None or not account.startswith("pid:") or int(account[4:]) not in song["pids"]:
            return None
        if not self.cfg.get("service_token_key"):
            return None
        path = os.path.join(self.cfg["private_dir"], song["file"])
        # dead_link_size: answer with a link that 404s, on purpose. A failed download makes the
        # game fall back to its bundled global/ujk/error.ujk (error record at 6448, 0x0227B5B4).
        size = song.get("dead_link_size")
        if size is None:
            if not os.path.isfile(path):
                return None
            size = os.path.getsize(path)
        expires = int(time.time()) + self.cfg["private_link_seconds"]
        sig = private_link_signature(self.cfg, song["file"], expires)
        url = "%s/content/private/%d/%s/%s" % (self.content.public_url, expires, sig, urllib.parse.quote(song["file"]))
        return {"url": url, "size": size, "id": self.content.md5(path)[:32] if os.path.isfile(path) else ""}

    def music_url(self, p, a):
        sid = p.get("sid")
        item = self._private_song(sid, a) or self._custom_song(sid, a) or self.content.lookup("music", sid=sid)
        return _url_reply(item)

    def lyrics_url(self, p, a):
        return _url_reply(self.content.lookup("lyrics", sid=p.get("sid")))

    def movie_url(self, p, a):
        return _url_reply(self.content.lookup("movie", mid=p.get("mid"), mno=p.get("mno")))

    def _bundled_reply(self, item, current_id=BUNDLED_ID):
        if item is not None:
            return _id_url_reply(item)
        url = self.content.public_url + "/content/none"
        return Reply().ok().line(current_id, 32).u32(0).line(url, 512).bytes()

    def db_url(self, p, a):
        # 0x020509E4: the game skips the download when this id equals the one in its save, and
        # that is empty on a console that never downloaded a database.
        item = self._private_item(self.cfg["private_db"], a)
        return self._bundled_reply(item if item is not None else self.content.lookup("db"), "")

    def movie_db_url(self, p, a):
        return self._bundled_reply(self.content.lookup("movie_db"), "")

    def db_patch_list(self, p, a):
        ids = self.content.patch_list(p.get("mdid"))
        r = Reply().ok().u16(len(ids))
        for i in ids:
            r.line(i, 32)
        return r.bytes()

    def db_patch_url(self, p, a):
        return _url_reply(self.content.lookup("patch", pdid=p.get("pdid")))

    def first_line_db_url(self, p, a):
        item = self._private_item(self.cfg["private_first_line"], a)
        return _url_reply(item if item is not None else self.content.lookup("first_line_db", dbid=p.get("dbid")))

    def feature_url(self, p, a):
        return self._bundled_reply(self.content.lookup("feature", lid=p.get("lid"), cid=p.get("cid")))

    def magazine_url(self, p, a):
        return self._bundled_reply(self.content.lookup("magazine", lid=p.get("lid"), cid=p.get("cid")))

    def urgent_info(self, p, a):
        # Both GetUrgentInfoURL and GetUrgentInfo send type NI0002; a leading
        # u8 of 0 means "nothing to show" for both parsers.
        item = self.content.lookup("urgent")
        if item is None:
            return Reply().ok().u8(0).bytes()
        return Reply().ok().u8(1).u32(item["size"]).line(item["url"], 512).bytes()

    # --- logs --------------------------------------------------------------
    def send_log(self, p, a):
        return Reply().ok().bytes()
