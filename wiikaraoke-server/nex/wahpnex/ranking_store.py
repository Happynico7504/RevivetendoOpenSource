"""
SQLite-backed NEX ranking (protocol 112) semantics, matching NEX 3.2.1 behaviour:

  orderBy     0 = ascending (lower better), 1 = descending (higher better)
              Wii Karaoke U always sends 1 (score = singing points x 1000).
  updateMode  0 = keep best, 1 = always overwrite
  orderCalculation 0 = "1224" competition ranking, 1 = "1234" ordinal (ties: earlier, then lower pid)
  groupIndex/groupNum  filter on groups[groupIndex] == groupNum (255 = no filter).
              Wii Karaoke U puts the console country (cid) in group 0.
  timeScope   0/1 = game-specific periods, 2 = all time (configurable below)
  rankingMode 0 range, 1 around self, 2 friends range, 3 friends around self, 4 self only
"""
import sqlite3
import threading
import time
from datetime import datetime, timedelta, timezone

SCHEMA = """
CREATE TABLE IF NOT EXISTS scores(
  pid INTEGER, unique_id INTEGER, category INTEGER, score INTEGER, order_by INTEGER,
  groups BLOB, param INTEGER, updated REAL, PRIMARY KEY(pid, unique_id, category));
CREATE TABLE IF NOT EXISTS common_data(
  pid INTEGER, unique_id INTEGER, data BLOB, updated REAL, PRIMARY KEY(pid, unique_id));
CREATE TABLE IF NOT EXISTS unique_ids(
  unique_id INTEGER PRIMARY KEY, pid INTEGER, created REAL);
CREATE INDEX IF NOT EXISTS scores_cat ON scores(category, score);
"""


def scope_start(scope_cfg, now=None):
    now = now or datetime.now(timezone.utc)
    if scope_cfg == "day":
        return now.replace(hour=0, minute=0, second=0, microsecond=0).timestamp()
    if scope_cfg == "week":
        d = now - timedelta(days=now.weekday())
        return d.replace(hour=0, minute=0, second=0, microsecond=0).timestamp()
    if scope_cfg == "month":
        return now.replace(day=1, hour=0, minute=0, second=0, microsecond=0).timestamp()
    if isinstance(scope_cfg, (int, float)):
        return time.time() - scope_cfg
    return 0.0                                     # "all"


class Entry:
    __slots__ = ("pid", "unique_id", "category", "score", "order_by", "groups", "param", "updated", "order")

    def __init__(self, row):
        (self.pid, self.unique_id, self.category, self.score, self.order_by,
         g, self.param, self.updated) = row
        self.groups = bytes(g or b"")
        self.order = 0


class RankingStore:
    def __init__(self, path, time_scopes=None):
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.executescript(SCHEMA)
        self.lock = threading.Lock()
        self.time_scopes = time_scopes or {0: "all", 1: "all", 2: "all"}

    # ---- writes -----------------------------------------------------------
    def upload_score(self, pid, uid, category, score, order_by, update_mode, groups, param):
        with self.lock:
            row = self.db.execute("SELECT score FROM scores WHERE pid=? AND unique_id=? AND category=?",
                                  (pid, uid, category)).fetchone()
            if row is not None and update_mode == 0:
                better = score > row[0] if order_by == 1 else score < row[0]
                if not better:
                    # keep the old score but refresh groups/param like NEX does
                    self.db.execute("UPDATE scores SET groups=?, param=? WHERE pid=? AND unique_id=? AND category=?",
                                    (bytes(groups), param, pid, uid, category))
                    self.db.commit()
                    return False
            self.db.execute("INSERT OR REPLACE INTO scores VALUES(?,?,?,?,?,?,?,?)",
                            (pid, uid, category, score, order_by, bytes(groups), param, time.time()))
            self.db.commit()
            return True

    def delete_score(self, pid, uid, category):
        with self.lock:
            self.db.execute("DELETE FROM scores WHERE pid=? AND unique_id=? AND category=?", (pid, uid, category))
            self.db.commit()

    def delete_all(self, pid, uid):
        with self.lock:
            self.db.execute("DELETE FROM scores WHERE pid=? AND unique_id=?", (pid, uid))
            self.db.commit()

    def set_common(self, pid, uid, data):
        with self.lock:
            self.db.execute("INSERT OR REPLACE INTO common_data VALUES(?,?,?,?)", (pid, uid, bytes(data), time.time()))
            self.db.commit()

    def delete_common(self, pid, uid):
        with self.lock:
            self.db.execute("DELETE FROM common_data WHERE pid=? AND unique_id=?", (pid, uid))
            self.db.commit()

    def get_common(self, pid, uid):
        r = self.db.execute("SELECT data FROM common_data WHERE pid=? AND unique_id=?", (pid, uid)).fetchone()
        if r is None and uid == 0:
            r = self.db.execute("SELECT data FROM common_data WHERE pid=? ORDER BY updated DESC", (pid,)).fetchone()
        return None if r is None else bytes(r[0])

    def change_attributes(self, pid, uid, category, flag, groups, param):
        with self.lock:
            q = "SELECT pid,unique_id,category,groups FROM scores WHERE pid=? AND unique_id=?"
            args = [pid, uid]
            if category is not None:
                q += " AND category=?"; args.append(category)
            for p, u, c, g in self.db.execute(q, args).fetchall():
                g = bytearray(g or b"")
                for i in range(4):
                    if flag & (1 << i):
                        while len(g) <= i:
                            g.append(0)
                        g[i] = groups[i] if i < len(groups) else 0
                newp = param if flag & 0x10 else None
                self.db.execute("UPDATE scores SET groups=?, param=COALESCE(?, param) WHERE pid=? AND unique_id=? AND category=?",
                                (bytes(g), newp, p, u, c))
            self.db.commit()

    def new_unique_id(self, pid):
        import random
        with self.lock:
            while True:
                uid = random.randint(1, (1 << 63) - 1)
                if not self.db.execute("SELECT 1 FROM unique_ids WHERE unique_id=?", (uid,)).fetchone():
                    break
            self.db.execute("INSERT INTO unique_ids VALUES(?,?,?)", (uid, pid, time.time()))
            self.db.commit()
            return uid

    # ---- queries ----------------------------------------------------------
    def _sorted(self, category, op):
        """op = dict(order_calc, group_index, group_num, time_scope) -> ranked list."""
        since = scope_start(self.time_scopes.get(op["time_scope"], "all"))
        rows = self.db.execute("SELECT * FROM scores WHERE category=? AND updated>=?", (category, since)).fetchall()
        ents = [Entry(r) for r in rows]
        gi, gn = op["group_index"], op["group_num"]
        if gi != 255:
            ents = [e for e in ents if len(e.groups) > gi and e.groups[gi] == gn]
        desc = (ents[0].order_by == 1) if ents else True
        ents.sort(key=lambda e: ((-e.score if desc else e.score), e.updated, e.pid))
        prev, rank = None, 0
        for i, e in enumerate(ents):
            if op["order_calc"] == 1:
                e.order = i + 1
            else:
                if e.score != prev:
                    rank, prev = i + 1, e.score
                e.order = rank
        return ents

    def ranking(self, mode, category, op, pid, uid, friends=None):
        ents = self._sorted(category, op)
        total = len(ents)
        length = op["length"] or 10
        mine = [e for e in ents if e.pid == pid and (uid == 0 or e.unique_id == uid)]
        if mode == 0:                                         # range
            sel = ents[op["offset"]:op["offset"] + length]
        elif mode in (1, 3):                                  # around self
            pool = ents if mode == 1 else [e for e in ents if e.pid == pid or (friends and e.pid in friends)]
            if not mine:
                return [], total
            idx = pool.index(mine[0])
            start = max(0, min(idx - (length - 1) // 2, len(pool) - length))
            sel = pool[start:start + length]
        elif mode == 2:                                       # friends range
            pool = [e for e in ents if e.pid == pid or (friends and e.pid in friends)]
            sel = pool[op["offset"]:op["offset"] + length]
        else:                                                 # 4: own
            sel = mine
        return sel, total

    def by_pids(self, pids, category, op, uid=0):
        ents = self._sorted(category, op)
        return [e for e in ents if e.pid in pids], len(ents)

    def by_unique_ids(self, uids, category, op):
        ents = self._sorted(category, op)
        return [e for e in ents if e.unique_id in uids], len(ents)

    def approx_order(self, category, op, score):
        ents = self._sorted(category, op)
        desc = (ents[0].order_by == 1) if ents else True
        better = sum(1 for e in ents if (e.score > score if desc else e.score < score))
        return better + 1

    def stats(self, category, op, flags):
        ents = self._sorted(category, op)
        sc = [e.score for e in ents]
        out = [0.0] * 5
        if sc:
            if flags & 1: out[0] = float(len(sc))
            if flags & 2: out[1] = float(sum(sc))
            if flags & 4: out[2] = float(min(sc))
            if flags & 8: out[3] = float(max(sc))
            if flags & 0x10: out[4] = float(sum(sc)) / len(sc)
        return out

    # ---- web views --------------------------------------------------------
    def categories(self):
        return self.db.execute("SELECT category, COUNT(*), MAX(score) FROM scores GROUP BY category ORDER BY COUNT(*) DESC").fetchall()
