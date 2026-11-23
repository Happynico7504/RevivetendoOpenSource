"""
/rankings - read-only web view of the NEX leaderboards (nex/nex_rankings.sqlite3).

Wii Karaoke U uploads one entry per song: category = song id (wiiudb.dat),
score = singing points x 1000, group 0 = console country (cafe.cntry_reg).
"""
import os
import sqlite3
import sys
from datetime import datetime, timezone

from flask import Blueprint, abort, render_template_string

COUNTRIES = {1: "Japan", 18: "Canada", 49: "United States", 64: "Albania", 65: "Australia", 66: "Austria",
             67: "Belgium", 70: "Bulgaria", 71: "Croatia", 72: "Cyprus", 73: "Czech Republic", 74: "Denmark",
             75: "Estonia", 76: "Finland", 77: "France", 78: "Germany", 79: "Greece", 80: "Hungary",
             81: "Iceland", 82: "Ireland", 83: "Italy", 84: "Latvia", 86: "Liechtenstein", 87: "Lithuania",
             88: "Luxembourg", 90: "Malta", 94: "Netherlands", 95: "New Zealand", 96: "Norway", 97: "Poland",
             98: "Portugal", 99: "Romania", 100: "Russia", 101: "Serbia", 102: "Slovakia", 103: "Slovenia",
             104: "South Africa", 105: "Spain", 107: "Sweden", 108: "Switzerland", 109: "Turkey",
             110: "United Kingdom"}

PAGE = """<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Karaoke Rankings</title><style>
body{margin:0;font-family:Arial,Helvetica,sans-serif;background:#f3f0ea;color:#222}
.bar{background:#e4002b;color:#fff;padding:14px 18px;font-size:20px;font-weight:bold}
.wrap{max-width:820px;margin:0 auto;padding:16px}
table{width:100%;border-collapse:collapse;background:#fff;border:1px solid #ddd;border-radius:8px}
td,th{padding:8px 10px;border-bottom:1px solid #eee;text-align:left;font-size:14px}
th{background:#faf7f2}.num{text-align:right;font-variant-numeric:tabular-nums}
a{color:#b5001f;text-decoration:none}.muted{color:#777;font-size:13px}
</style></head><body><div class="bar">Wii Karaoke U &mdash; Rankings</div><div class="wrap">
{% if song %}<p><a href="{{url_for('rankings.index')}}">&larr; all songs</a></p><h2>{{song}}</h2>
<table><tr><th>#</th><th>Singer</th><th>Country</th><th class="num">Score</th><th>Date</th></tr>
{% for r in rows %}<tr><td>{{r.rank}}</td><td>{{r.name}}</td><td>{{r.country}}</td><td class="num">{{r.score}}</td><td>{{r.date}}</td></tr>{% endfor %}
</table>{% else %}
{% if not cats %}<p class="muted">No scores uploaded yet.</p>{% else %}
<table><tr><th>Song</th><th class="num">Singers</th><th class="num">Top score</th></tr>
{% for c in cats %}<tr><td><a href="{{url_for('rankings.song', category=c.id)}}">{{c.title}}</a></td><td class="num">{{c.n}}</td><td class="num">{{c.top}}</td></tr>{% endfor %}
</table>{% endif %}{% endif %}</div></body></html>"""


def mii_name(common):
    """Best effort: find a Mii nickname (UTF-16, up to 10 chars) in the common data."""
    best = ""
    for enc in ("utf-16-le", "utf-16-be"):
        for off in range(0, max(0, len(common) - 4), 2):
            chunk = common[off:off + 20]
            try:
                s = chunk.decode(enc).split("\0")[0]
            except UnicodeDecodeError:
                continue
            if 2 <= len(s) <= 10 and all(c.isprintable() for c in s) and len(s) > len(best):
                if sum(c.isalnum() for c in s) >= len(s) // 2 + 1:
                    best = s
    return best


def make_rankings(cfg, here):
    bp = Blueprint("rankings", __name__)
    db_path = cfg.get("nex_database") or ""
    db_path = db_path if os.path.isabs(db_path) else os.path.join(here, db_path)
    song_db = cfg.get("song_db") or ""
    song_db = song_db if os.path.isabs(song_db) else os.path.join(here, song_db)
    cache = {}

    def songs():
        if "songs" not in cache:
            cache["songs"] = {}
            if os.path.exists(song_db):
                sys.path.insert(0, os.path.join(here, "tools"))
                from wiiudb import WiiUDB
                try:
                    cache["songs"] = WiiUDB(song_db).songs()
                except Exception:
                    pass
        return cache["songs"]

    def title(cat):
        t = songs().get(cat)
        return "%s - %s" % t if t else "Song %d" % cat

    def db():
        if not os.path.exists(db_path):
            return None
        return sqlite3.connect("file:%s?mode=ro" % db_path, uri=True)

    @bp.route("/rankings")
    def index():
        con = db()
        cats = []
        if con:
            for cat, n, top in con.execute("SELECT category, COUNT(*), MAX(score) FROM scores GROUP BY category ORDER BY COUNT(*) DESC, category"):
                cats.append({"id": cat, "title": title(cat), "n": n, "top": "%.3f" % (top / 1000)})
        return render_template_string(PAGE, cats=cats, song=None)

    @bp.route("/rankings/<int:category>")
    def song(category):
        con = db()
        if not con:
            abort(404)
        rows = []
        rank, prev = 0, None
        q = ("SELECT s.pid, s.score, s.groups, s.updated, c.data FROM scores s LEFT JOIN common_data c "
             "ON c.pid=s.pid AND c.unique_id=s.unique_id WHERE s.category=? ORDER BY s.score DESC, s.updated LIMIT 100")
        for i, (pid, score, groups, updated, common) in enumerate(con.execute(q, (category,))):
            if score != prev:
                rank, prev = i + 1, score
            g = bytes(groups or b"")
            rows.append({"rank": rank, "name": mii_name(bytes(common or b"")) or "PID %d" % pid,
                         "country": COUNTRIES.get(g[0], str(g[0])) if g else "",
                         "score": "%.3f" % (score / 1000),
                         "date": datetime.fromtimestamp(updated, timezone.utc).strftime("%Y-%m-%d")})
        return render_template_string(PAGE, rows=rows, song=title(category), cats=None)

    return bp
