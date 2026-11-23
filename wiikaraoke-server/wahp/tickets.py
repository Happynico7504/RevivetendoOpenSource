"""
Ticket ("service period") bookkeeping.

How tickets worked originally
-----------------------------
* The app is free; singing online songs needs a Ticket.
* "Ticket Purchase" in the app calls SYSSwitchToEShop() with an empty
  argument block (0x02368ef8 / 0x023a3c84), i.e. it jumps to the app's own
  Nintendo eShop page, where the tickets were sold. A SendLog (LG000x, ltyp
  "es", key "route") is sent right before the jump.
* When the user comes back, the app asks its own server:
      TE0001  GetPurchaseHistory  -> u32
      BA0001  GetServicePeriod    -> u8 status, expiry, server-now, u8 permanent
  and the server (which talked to Nintendo's shop backend) answered with the
  period bought.
* EU line-up (Nintendo UK press release, Sept 2013):
      1-hour  £1.79 / €1.99
      24-hour £4.49 / €4.99
      30-day  £13.49 / €14.99
      + one free 1-hour trial per console, redeemable only before any other
        ticket purchase.

The eShop is gone, so this module replaces Nintendo's purchase records with a
local SQLite store and the web "Ticket Shop" (shop.py) replaces the eShop page.
Tickets stack: a new ticket extends the current expiry, or starts now.
"""
import sqlite3
import threading
import time
from datetime import datetime, timedelta
from zoneinfo import ZoneInfo

PRODUCTS = {
    "trial_1h": {"name": "Free Trial Ticket (1 hour)", "seconds": 3600,
                 "price": {"GBP": 0, "EUR": 0}, "trial": True},
    "ticket_1h": {"name": "1-Hour Ticket", "seconds": 3600,
                  "price": {"GBP": 179, "EUR": 199}},
    "ticket_24h": {"name": "24-Hour Ticket", "seconds": 86400,
                   "price": {"GBP": 449, "EUR": 499}},
    "ticket_30d": {"name": "30-Day Ticket", "seconds": 30 * 86400,
                   "price": {"GBP": 1349, "EUR": 1499}},
}

SCHEMA = """
CREATE TABLE IF NOT EXISTS accounts(
  key TEXT PRIMARY KEY, created REAL, balance_cents INTEGER DEFAULT 0,
  currency TEXT, timezone TEXT, last_seen REAL, last_cid TEXT, last_lid TEXT, last_ip TEXT);
CREATE TABLE IF NOT EXISTS purchases(
  id INTEGER PRIMARY KEY AUTOINCREMENT, account TEXT, product TEXT,
  name TEXT, seconds INTEGER, price_cents INTEGER, currency TEXT,
  purchased REAL, starts REAL, expires REAL, source TEXT);
CREATE TABLE IF NOT EXISTS logs(
  id INTEGER PRIMARY KEY AUTOINCREMENT, at REAL, account TEXT, type TEXT, params TEXT);
"""


class Store:
    def __init__(self, path, cfg):
        self.cfg = cfg
        self.lock = threading.Lock()
        self.db = sqlite3.connect(path, check_same_thread=False)
        self.db.row_factory = sqlite3.Row
        self.db.executescript(SCHEMA)
        try:
            self.db.execute("ALTER TABLE accounts ADD COLUMN last_ip TEXT")
        except sqlite3.OperationalError:
            pass

    # ---- accounts ---------------------------------------------------------
    def account(self, key, cid=None, lid=None, ip=None):
        with self.lock:
            row = self.db.execute("SELECT * FROM accounts WHERE key=?", (key,)).fetchone()
            now = time.time()
            if row is None:
                self.db.execute(
                    "INSERT INTO accounts(key,created,balance_cents,currency,timezone,last_seen,last_cid,last_lid,last_ip)"
                    " VALUES(?,?,?,?,?,?,?,?,?)",
                    (key, now, self.cfg["starting_funds_cents"], self.cfg["currency"],
                     self.cfg["timezone"], now, cid, lid, ip))
            elif cid is not None:
                self.db.execute("UPDATE accounts SET last_seen=?, last_cid=?, last_lid=?, last_ip=? WHERE key=?",
                                (now, cid, lid, ip, key))
            self.db.commit()
            return dict(self.db.execute("SELECT * FROM accounts WHERE key=?", (key,)).fetchone())

    def accounts_from_ip(self, ip, within=7 * 86400):
        return [dict(r) for r in self.db.execute(
            "SELECT * FROM accounts WHERE last_ip=? AND last_seen>? ORDER BY last_seen DESC",
            (ip, time.time() - within))]

    def accounts(self):
        return [dict(r) for r in self.db.execute("SELECT * FROM accounts ORDER BY last_seen DESC")]

    def known_pid(self, pid):
        """True if this PID has ever authenticated to this server (has an
        accounts row) - the generic PID gate for the public community-songs
        content path, which has no session/ticket context on a plain GET
        the way the game account endpoint does."""
        row = self.db.execute("SELECT 1 FROM accounts WHERE key=? LIMIT 1", ("pid:%s" % pid,)).fetchone()
        return row is not None

    def set_timezone(self, key, tz):
        ZoneInfo(tz)  # validate
        with self.lock:
            self.db.execute("UPDATE accounts SET timezone=? WHERE key=?", (tz, key)); self.db.commit()

    def add_funds(self, key, cents):
        with self.lock:
            self.db.execute("UPDATE accounts SET balance_cents=balance_cents+? WHERE key=?", (cents, key))
            self.db.commit()

    # ---- purchases --------------------------------------------------------
    def purchases(self, key):
        return [dict(r) for r in self.db.execute(
            "SELECT * FROM purchases WHERE account=? ORDER BY id", (key,))]

    def current_expiry(self, key):
        r = self.db.execute("SELECT MAX(expires) e FROM purchases WHERE account=?", (key,)).fetchone()
        return r["e"]

    def can_buy(self, key, product_id):
        p = PRODUCTS.get(product_id)
        if not p:
            return False, "Unknown item."
        hist = self.purchases(key)
        if p.get("trial"):
            if any(h["product"] == "trial_1h" for h in hist):
                return False, "The free trial ticket can only be redeemed once per console."
            if hist:
                return False, "The free trial must be redeemed before any other ticket."
        acct = self.account(key)
        price = p["price"][acct["currency"]]
        if self.cfg["require_funds"] and acct["balance_cents"] < price:
            return False, "Not enough funds. Add funds first."
        return True, ""

    def buy(self, key, product_id, source="shop"):
        ok, why = self.can_buy(key, product_id)
        if not ok:
            raise ValueError(why)
        p = PRODUCTS[product_id]
        acct = self.account(key)
        price = p["price"][acct["currency"]]
        with self.lock:
            now = time.time()
            cur = self.db.execute("SELECT MAX(expires) e FROM purchases WHERE account=?", (key,)).fetchone()["e"]
            start = max(now, cur or 0)
            exp = start + p["seconds"]
            self.db.execute(
                "INSERT INTO purchases(account,product,name,seconds,price_cents,currency,purchased,starts,expires,source)"
                " VALUES(?,?,?,?,?,?,?,?,?,?)",
                (key, product_id, p["name"], p["seconds"], price, acct["currency"], now, start, exp, source))
            if self.cfg["require_funds"]:
                self.db.execute("UPDATE accounts SET balance_cents=balance_cents-? WHERE key=?", (price, key))
            self.db.commit()
        return exp

    def grant(self, key, seconds, note="admin"):
        with self.lock:
            now = time.time()
            cur = self.db.execute("SELECT MAX(expires) e FROM purchases WHERE account=?", (key,)).fetchone()["e"]
            start = max(now, cur or 0)
            self.db.execute(
                "INSERT INTO purchases(account,product,name,seconds,price_cents,currency,purchased,starts,expires,source)"
                " VALUES(?,?,?,?,?,?,?,?,?,?)",
                (key, "grant", "Granted Ticket", seconds, 0, "", now, start, start + seconds, note))
            self.db.commit()

    def clear(self, key):
        with self.lock:
            self.db.execute("DELETE FROM purchases WHERE account=?", (key,)); self.db.commit()

    def log(self, key, typ, params):
        with self.lock:
            self.db.execute("INSERT INTO logs(at,account,type,params) VALUES(?,?,?,?)",
                            (time.time(), key, typ, params))
            self.db.commit()

    # ---- what the game is told -------------------------------------------
    def service_period(self, key):
        """-> (status, expiry_dt, now_dt, permanent) for BA0001.

        Game logic (0x02114488):
          status 1, permanent 0: valid if now <= expiry, else error 30 (INVALID_PERIOD)
          status 1, permanent 1: unlimited ticket
          status 2 / 0, permanent 0: error 20 / 22 (no usable ticket)
        """
        acct = self.account(key)
        tz = ZoneInfo(acct["timezone"] or self.cfg["timezone"])
        now = datetime.now(tz).replace(microsecond=0)
        if self.cfg["ticket_mode"] == "unlimited":
            return 1, now + timedelta(days=3650), now, 1
        exp = self.current_expiry(key)
        if exp and exp > now.timestamp():
            return 1, datetime.fromtimestamp(exp, tz).replace(microsecond=0), now, 0
        if exp:
            return self.cfg["status_expired"], datetime.fromtimestamp(exp, tz), now, 0
        return self.cfg["status_no_ticket"], now, now, 0
