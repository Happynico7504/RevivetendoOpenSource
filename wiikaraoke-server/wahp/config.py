import json
import os

DEFAULTS = {
    # Address the console reaches this server on. Used to build content URLs
    # returned to the game (music/lyrics/DB downloads).
    "public_url": "http://192.168.1.10:8383",
    "host": "0.0.0.0",
    "port": 8383,

    # Path the game requests (EU build). Kept identical to the original.
    "endpoint": "/control_Orchestra300_EU.php",

    # Timezone used for the YYYYMMDDhhmmss times sent to the console
    # (ticket expiry shown in-game). Accounts can override it in the shop.
    "timezone": "Europe/London",

    # How a console is tied to a ticket wallet.
    #   "token"  - by the account PID inside the atk service token (Pretendo-style
    #              token: u32 pid | u64 title | u64 issued | u64 expires | HMAC-SHA256).
    #              Falls back to "ip" if the token can't be read.
    #   "ip"     - by the console's public/LAN IP
    #   "single" - one shared wallet for everyone (simplest for home use)
    "identity": "token",
    # HMAC key of the account-server "server" entry with client_id
    # 471ac6f9b1368cba77a15da69d5b0241 (Pretendo calls it aes_key). Empty = don't verify.
    "service_token_key": "",
    # Optional: show NEX leaderboards on /rankings
    "nex_database": "nex/nex_rankings.sqlite3",
    "song_db": "content/wiiudb.dat",
    "trust_proxy_headers": False,

    # "authentic"  - tickets must be bought in the shop, time runs out
    # "unlimited"  - every console gets the in-game *permanent* ticket
    #                (top_ticket_permanent UI; server sends permanent flag)
    "ticket_mode": "authentic",

    # Ticket shop currency ("GBP" or "EUR") and simulated eShop funds.
    "currency": "GBP",
    "require_funds": True,
    "starting_funds_cents": 0,

    # Returned to BA0001 when the account has *never* had a ticket / had one
    # that expired. 0 and 2 both make the game show its "no ticket" error.
    "status_no_ticket": 0,
    "status_expired": 2,

    # SendToken (SA0001) token lifetime the console is told about (hours).
    "token_lifetime_hours": 6,

    "content_dir": "content",
    # Songs only certain accounts may download: {"<sid>": {"file": "<name in private_dir>", "pids": [pid, ...]}}.
    # Nobody else can tell they exist: they get the normal "no record".
    "private_dir": "private",
    "private_songs": {},
    # A first-line database (GF0001) for certain accounts only: {"file": "...", "pids": [pid, ...]}.
    "private_first_line": None,
    # The same for the song database (GD0001).
    "private_db": None,
    "private_link_seconds": 3600,
    "database": "wahp.sqlite3",
    "admin_key": "change-me",
    "log_requests": True,
}


def load(path=None):
    cfg = dict(DEFAULTS)
    path = path or os.environ.get("WAHP_CONFIG", "config.json")
    if os.path.exists(path):
        with open(path, "r", encoding="utf-8") as f:
            cfg.update(json.load(f))
    # Secrets come from the environment (.env, sourced by start.sh), never from the repo.
    for key, env in (("service_token_key", "WAHP_SERVICE_TOKEN_KEY"), ("admin_key", "WAHP_ADMIN_KEY")):
        if os.environ.get(env):
            cfg[key] = os.environ[env]
    return cfg
