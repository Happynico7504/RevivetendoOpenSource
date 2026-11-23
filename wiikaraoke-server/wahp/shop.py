"""
Web Ticket Shop - stands in for the app's Nintendo eShop page.

Open http://<server>/shop from a PC/phone on the same network as the console,
or straight from the Wii U Internet Browser. Plain HTML forms only, so it
works in the Wii U browser.
"""
import time
from datetime import datetime
from zoneinfo import ZoneInfo, available_timezones

from flask import Blueprint, abort, redirect, render_template_string, request, url_for

from .tickets import PRODUCTS

SYMBOL = {"GBP": "£", "EUR": "€"}

PAGE = """<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Ticket Shop</title>
<style>
 body{margin:0;font-family:Arial,Helvetica,sans-serif;background:#f3f0ea;color:#222}
 .bar{background:#e4002b;color:#fff;padding:14px 18px;font-size:20px;font-weight:bold}
 .bar small{font-weight:normal;font-size:13px;opacity:.85;display:block}
 .wrap{max-width:760px;margin:0 auto;padding:16px}
 .card{background:#fff;border:1px solid #ddd;border-radius:8px;padding:14px 16px;margin:0 0 14px}
 .row{display:flex;justify-content:space-between;align-items:center;gap:10px;flex-wrap:wrap}
 .tk{border-left:6px solid #e4002b}
 .name{font-size:17px;font-weight:bold}
 .price{font-size:18px;font-weight:bold}
 button{background:#e4002b;color:#fff;border:0;border-radius:18px;padding:9px 18px;font-size:15px;cursor:pointer}
 button.alt{background:#555}
 button[disabled]{background:#bbb}
 .status{font-size:15px}.ok{color:#0a7d2c;font-weight:bold}.no{color:#b00;font-weight:bold}
 .msg{background:#fff6d6;border:1px solid #e8d48a;padding:10px;border-radius:6px;margin-bottom:14px}
 table{width:100%;border-collapse:collapse;font-size:14px}td,th{padding:6px;border-bottom:1px solid #eee;text-align:left}
 select{font-size:14px;padding:4px}
 .muted{color:#777;font-size:13px}
</style></head><body>
<div class="bar">Wii Karaoke U &mdash; Ticket Shop<small>Revival server &middot; console: {{acct.key}}</small></div>
<div class="wrap">
{% if msg %}<div class="msg">{{msg}}</div>{% endif %}
{% if others %}<div class="card"><form method="get"><div class="row"><div class="muted">Other consoles/users seen from your network:</div>
<div><select name="acct">{% for o in others %}<option>{{o}}</option>{% endfor %}</select> <button class="alt">Switch</button></div></div></form></div>{% endif %}
<div class="card">
 <div class="row"><div class="status">
 {% if mode == 'unlimited' %}<span class="ok">Unlimited ticket (server setting)</span>
 {% elif active %}<span class="ok">Ticket active</span> &mdash; until <b>{{expiry}}</b> ({{remaining}} left)
 {% else %}<span class="no">No active ticket</span>{% endif %}
 </div>
 {% if require_funds %}<div><b>Funds:</b> {{sym}}{{'%.2f' % (acct.balance_cents/100)}}</div>{% endif %}
 </div>
</div>
{% for pid, p in products %}
<div class="card tk"><form method="post" action="{{url_for('shop.buy')}}"><div class="row">
 <div><div class="name">{{p.name}}</div><div class="muted">{{p.desc}}</div></div>
 <div class="row"><span class="price">{% if p.cents == 0 %}Free{% else %}{{sym}}{{'%.2f' % (p.cents/100)}}{% endif %}</span>
 <input type="hidden" name="product" value="{{pid}}"><input type="hidden" name="acct" value="{{acct.key}}">
 <button {% if not p.ok %}disabled title="{{p.why}}"{% endif %}>{% if p.cents == 0 %}Redeem{% else %}Purchase{% endif %}</button></div>
</div></form></div>
{% endfor %}
{% if require_funds %}
<div class="card"><form method="post" action="{{url_for('shop.funds')}}"><input type="hidden" name="acct" value="{{acct.key}}"><div class="row">
 <div><div class="name">Add Funds</div><div class="muted">Simulated balance &mdash; no real money involved.</div></div>
 <div><select name="cents">{% for c in [500,1000,2000,5000] %}<option value="{{c}}">{{sym}}{{'%.2f' % (c/100)}}</option>{% endfor %}</select>
 <button class="alt">Add</button></div></div></form></div>
{% endif %}
<div class="card"><form method="post" action="{{url_for('shop.tz')}}"><input type="hidden" name="acct" value="{{acct.key}}"><div class="row">
 <div><div class="name">Console time zone</div><div class="muted">Used for the expiry date shown in-game.</div></div>
 <div><select name="tz">{% for z in zones %}<option {% if z == acct.timezone %}selected{% endif %}>{{z}}</option>{% endfor %}</select>
 <button class="alt">Save</button></div></div></form></div>
<div class="card"><div class="name">Purchase history</div>
{% if history %}<table><tr><th>Date</th><th>Item</th><th>Price</th><th>Valid until</th></tr>
{% for h in history %}<tr><td>{{h.date}}</td><td>{{h.name}}</td><td>{{h.price}}</td><td>{{h.until}}</td></tr>{% endfor %}</table>
{% else %}<p class="muted">No purchases yet.</p>{% endif %}</div>
<p class="muted">After buying, go back to the app (or restart it) &mdash; it re-checks the ticket with the server.</p>
</div></body></html>"""

EMPTY = """<!doctype html><html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Ticket Shop</title>
<style>body{font-family:Arial;background:#f3f0ea;margin:0}.bar{background:#e4002b;color:#fff;padding:14px 18px;font-size:20px;font-weight:bold}.wrap{max-width:640px;margin:20px auto;padding:0 16px}</style></head>
<body><div class="bar">Wii Karaoke U &mdash; Ticket Shop</div><div class="wrap"><p>No Wii U has talked to this server from your network yet.</p>
<p>Start Wii Karaoke U once (so the console signs in), then reload this page from a device on the same network &mdash; or open it in the Wii U Internet Browser.</p></div></body></html>"""

ADMIN = """<!doctype html><html><head><meta charset="utf-8"><title>WAHP admin</title>
<style>body{font-family:Arial;margin:20px}table{border-collapse:collapse}td,th{border:1px solid #ccc;padding:5px 8px}</style></head><body>
<h2>Accounts</h2><table><tr><th>key</th><th>last seen</th><th>cid/lid</th><th>expiry</th><th>purchases</th><th>actions</th></tr>
{% for a in accts %}<tr><td>{{a.key}}</td><td>{{a.seen}}</td><td>{{a.last_cid}}/{{a.last_lid}}</td><td>{{a.exp}}</td><td>{{a.n}}</td>
<td><form method="post" style="display:inline"><input type="hidden" name="key" value="{{a.key}}">
<select name="hours"><option value="1">1h</option><option value="24">24h</option><option value="720">30d</option></select>
<button name="act" value="grant">grant</button><button name="act" value="clear">clear</button></form></td></tr>{% endfor %}</table>
<h2>Recent logs (SendLog)</h2><table><tr><th>time</th><th>account</th><th>type</th><th>params</th></tr>
{% for l in logs %}<tr><td>{{l.t}}</td><td>{{l.account}}</td><td>{{l.type}}</td><td style="font-family:monospace;font-size:12px">{{l.params}}</td></tr>{% endfor %}</table>
</body></html>"""

ZONES = sorted(z for z in available_timezones() if "/" in z and not z.startswith(("Etc", "SystemV", "posix", "right")))


def _fmt(ts, tz):
    return datetime.fromtimestamp(ts, ZoneInfo(tz)).strftime("%d/%m/%Y %H:%M")


def _left(sec):
    sec = int(sec)
    d, r = divmod(sec, 86400); h, r = divmod(r, 3600); m = r // 60
    return (f"{d}d " if d else "") + f"{h}h {m:02d}m"


def make_shop(cfg, store):
    bp = Blueprint("shop", __name__)
    from .identity import client_ip

    def candidates():
        ip = client_ip(request, cfg)
        if cfg["identity"] == "single":
            return ["default"]
        found = [a["key"] for a in store.accounts_from_ip(ip)]
        if cfg["identity"] == "ip" and ("ip:" + ip) not in found:
            found.append("ip:" + ip)
        return found

    def account_key(_cfg=None):
        c = candidates()
        want = request.values.get("acct") or request.cookies.get("acct")
        if want in c:
            return want
        return c[0] if c else None

    @bp.route("/")
    def index():
        return redirect(url_for("shop.page"))

    @bp.route("/shop")
    def page():
        key = account_key(cfg)
        if key is None:
            return render_template_string(EMPTY), 200
        acct = store.account(key)
        tz = acct["timezone"] or cfg["timezone"]
        cur = acct["currency"]
        exp = store.current_expiry(key)
        now = time.time()
        products = []
        for pid, p in PRODUCTS.items():
            ok, why = store.can_buy(key, pid)
            if p.get("trial") and not ok and "once" in why:
                continue
            desc = {"trial_1h": "One per console. Must be redeemed before any other ticket.",
                    "ticket_1h": "Sing for 1 hour.", "ticket_24h": "Sing for 24 hours.",
                    "ticket_30d": "Sing for 30 days."}.get(pid, "")
            products.append((pid, {"name": p["name"], "cents": p["price"][cur], "ok": ok, "why": why, "desc": desc}))
        hist = [{"date": _fmt(h["purchased"], tz), "name": h["name"],
                 "price": ("Free" if not h["price_cents"] else f"{SYMBOL.get(h['currency'], '')}{h['price_cents']/100:.2f}"),
                 "until": _fmt(h["expires"], tz)} for h in reversed(store.purchases(key))]
        return render_template_string(
            PAGE, acct=acct, products=products, sym=SYMBOL.get(cur, ""), history=hist,
            active=bool(exp and exp > now), expiry=_fmt(exp, tz) if exp else "",
            remaining=_left(exp - now) if exp and exp > now else "", mode=cfg["ticket_mode"],
            require_funds=cfg["require_funds"], zones=ZONES, msg=request.args.get("msg"),
            others=[k for k in candidates() if k != key])

    @bp.route("/shop/buy", methods=["POST"])
    def buy():
        key = account_key(cfg)
        if key is None:
            abort(400)
        pid = request.form.get("product", "")
        try:
            store.buy(key, pid)
            msg = f"Thank you! {PRODUCTS[pid]['name']} added."
        except ValueError as e:
            msg = str(e)
        return redirect(url_for("shop.page", msg=msg, acct=account_key(cfg)))

    @bp.route("/shop/funds", methods=["POST"])
    def funds():
        cents = int(request.form.get("cents", "0"))
        if cents not in (500, 1000, 2000, 5000):
            abort(400)
        if account_key(cfg) is None:
            abort(400)
        store.add_funds(account_key(cfg), cents)
        return redirect(url_for("shop.page", msg="Funds added.", acct=account_key(cfg)))

    @bp.route("/shop/tz", methods=["POST"])
    def tz():
        try:
            store.set_timezone(account_key(cfg), request.form.get("tz", ""))
            msg = "Time zone saved."
        except Exception:
            msg = "Unknown time zone."
        return redirect(url_for("shop.page", msg=msg, acct=account_key(cfg)))

    @bp.route("/admin", methods=["GET", "POST"])
    def admin():
        if request.args.get("key") != cfg["admin_key"] or cfg["admin_key"] == "change-me":
            abort(403)
        if request.method == "POST":
            k = request.form["key"]
            if request.form.get("act") == "grant":
                store.grant(k, int(request.form.get("hours", "1")) * 3600)
            elif request.form.get("act") == "clear":
                store.clear(k)
        accts = []
        for a in store.accounts():
            tzn = a["timezone"] or cfg["timezone"]
            e = store.current_expiry(a["key"])
            accts.append({**a, "seen": _fmt(a["last_seen"], tzn), "exp": _fmt(e, tzn) if e else "-",
                          "n": len(store.purchases(a["key"]))})
        logs = [{**dict(r), "t": _fmt(r["at"], cfg["timezone"])} for r in
                store.db.execute("SELECT * FROM logs ORDER BY id DESC LIMIT 100")]
        return render_template_string(ADMIN, accts=accts, logs=logs)

    return bp
