"""
Content lookup for the URL-returning commands.

The original server only handed out *URLs + sizes* (and a 32-char id for the
databases); the game then downloads the files itself with libcurl (byte-range
requests, "%u-%u"). The JOYSOUND song data, lyrics, movies and song database
are not in the RPX - you have to supply them. Put files under content/ and
describe them in content/manifest.json:

{
  "db":          "db/main.bin",                 GD0001 GetDBURL
  "movie_db":    "db/movie.bin",                GM0001 GetMovieDBURL
  "first_line_db": "db/firstline_{dbid}.bin",   GF0001 GetFirstLineDBURL(dbid)
  "patch_list":  {"*": ["<pdid>", ...]},        GL0001 GetDBPatchList(mdid)  (key = mdid or "*")
  "patch":       "db/patch/{pdid}.bin",         GP0001 GetDBPatchURL(pdid)
  "music":       "music/{sid}.bin",             SD0001 GetMusicURL(sid,rlog)   sid is %07u
  "lyrics":      "lyrics/{sid}.bin",            WO0001 GetLyrics(sid)
  "movie":       "movie/{mid}_{mno}.bin",       MD0001 GetMovieURL(mid,mno)
  "feature":     "feature/{lid}.bin",           FE0001 GetFeatureURL
  "magazine":    "magazine/{lid}.bin",          MA0001 GetMagazineURL(cid,lid)
  "urgent":      null                           NI0002 urgent notice (null = none)
}
Any entry may also be an object {"file": "...", "id": "<32 chars>"} to pin the
id; otherwise the id is the file's MD5 (hex, 32 chars - fits the 33-byte buffer).
"""
import hashlib
import json
import os
import re
import urllib.parse

_SAFE = re.compile(r"^[A-Za-z0-9_.\-]{1,64}$")


class Content:
    def __init__(self, root, public_url):
        self.root = os.path.abspath(root)
        self.public_url = public_url.rstrip("/")
        self._md5 = {}

    def manifest(self):
        p = os.path.join(self.root, "manifest.json")
        if not os.path.exists(p):
            return {}
        with open(p, "r", encoding="utf-8") as f:
            return json.load(f)

    def _resolve(self, entry, **fmt):
        if entry is None:
            return None, None
        pinned = None
        if isinstance(entry, dict):
            pinned = entry.get("id"); entry = entry.get("file")
        for k, v in fmt.items():
            if v is None or not _SAFE.match(str(v)):
                return None, None          # refuse path tricks from request params
        rel = entry.format(**fmt)
        full = os.path.abspath(os.path.join(self.root, rel))
        if not full.startswith(self.root + os.sep) or not os.path.isfile(full):
            return None, None
        return rel, pinned

    def lookup(self, key, **fmt):
        """-> dict(url, size, id) or None"""
        entry = self.manifest().get(key)
        rel, pinned = self._resolve(entry, **fmt)
        if rel is None:
            return None
        full = os.path.join(self.root, rel)
        size = os.path.getsize(full)
        return {"url": self.public_url + "/content/" + urllib.parse.quote(rel.replace(os.sep, "/")),
                "size": size, "id": (pinned or self.md5(full))[:32]}

    def patch_list(self, mdid):
        pl = self.manifest().get("patch_list") or {}
        return [str(x)[:32] for x in (pl.get(str(mdid)) or pl.get("*") or [])]

    def md5(self, full):
        st = os.stat(full)
        k = (full, st.st_mtime, st.st_size)
        if k not in self._md5:
            h = hashlib.md5()
            with open(full, "rb") as f:
                for chunk in iter(lambda: f.read(1 << 20), b""):
                    h.update(chunk)
            self._md5[k] = h.hexdigest()
        return self._md5[k]
