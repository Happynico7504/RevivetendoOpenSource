"""
NEX protocols used by Wii Karaoke U (from the DDL trees embedded in app.rpx, NEX 3.2.1):

 auth server:   TicketGranting (10)  Login / LoginEx / RequestTicket / GetPID / GetName
 secure server: SecureConnection (11) Register / RequestConnectionData / RequestURLs / RegisterEx / TestConnectivity
                Utility (110)        AcquireNexUniqueId
                Ranking (112)        methods 1-13
                Health (18), Monitoring (19), RemoteLogDevice (1)  (harmless stubs)
"""
import logging
import time

from .crypto import derive_key, make_ticket, open_internal_ticket, kerberos_decrypt
from .ranking_store import Entry
from .rmc import RMCError, SUCCESS
from .streams import StreamIn, StreamOut, nex_datetime, parse_station_url, station_url

log = logging.getLogger("nex")

SECURE_PID = 2          # "Quazal Rendez-Vous"
NAMES = {10: "TicketGranting", 11: "SecureConnection", 110: "Utility", 112: "Ranking",
         18: "Health", 19: "Monitoring", 1: "RemoteLogDevice"}


# --------------------------------------------------------------------------- auth
class Auth:
    def __init__(self, cfg, get_password):
        self.cfg = cfg
        self.get_password = get_password
        self.server_key = derive_key(cfg["secure_server_password"].encode(), SECURE_PID)
        self.build = cfg.get("build_name", "branch:origin/release/ngs/3.2.x build:3_2_1_2_0")

    def secure_url(self):
        return station_url("prudps", address=self.cfg["public_ip"], port=self.cfg["secure_port"],
                           CID=1, PID=SECURE_PID, sid=1, stream=10, type=2)

    def _ticket_for(self, pid):
        pw = self.get_password(pid)
        if pw is None:
            raise RMCError("RendezVous::InvalidUsername")
        return make_ticket(derive_key(pw.encode(), pid), pid, self.server_key)[0]

    def _login(self, conn, username, out, extra=None):
        try:
            pid = int(username)
            ticket = self._ticket_for(pid)
        except (ValueError, RMCError):
            log.warning("auth: login refused for %r (unknown user or no NEX password)", username)
            pid, ticket = None, None
        if ticket is None:
            out.result(0x80030064).pid(0).buffer(b"")
            out.string("").u32(0).string("")                # empty RVConnectionData
            out.string("")
            return
        conn.ctx["auth_pid"] = pid
        log.info("auth: pid %d logged in%s", pid, " (token %s...)" % extra[:12] if extra else "")
        out.result(SUCCESS).pid(pid).buffer(ticket)
        out.string(self.secure_url()).u32(0).string("")     # RVConnectionData (no special protocols)
        out.string(self.build)

    def login(self, conn, s, out):                         # 1
        self._login(conn, s.string(), out)

    def login_ex(self, conn, s, out):                      # 2
        user = s.string()
        token = None
        try:
            name, data = s.anydata()
            d = StreamIn(data)
            token = d.string()           # AuthenticationInfo.m_authToken
            ngs, b64type, sv = d.u32(), d.u8(), d.u32()
            log.debug("LoginEx %s %s ngs=%d type=%d serverVersion=%d", user, name, ngs, b64type, sv)
        except Exception:
            pass
        self._login(conn, user, out, token)

    def request_ticket(self, conn, s, out):                # 3
        src, tgt = s.pid(), s.pid()
        if tgt != SECURE_PID:
            raise RMCError("Core::AccessDenied")
        out.result(SUCCESS).buffer(self._ticket_for(src))

    def get_pid(self, conn, s, out):                       # 4
        name = s.string()
        out.pid(int(name) if name.isdigit() else 0)

    def get_name(self, conn, s, out):                      # 5
        out.string(str(s.pid()))

    def table(self):
        return {10: {1: self.login, 2: self.login_ex, 3: self.request_ticket, 4: self.get_pid, 5: self.get_name}}

    # PRUDP CONNECT payload on the secure server
    def secure_login(self, conn, payload):
        s = StreamIn(payload)
        ticket, req = s.buffer(), s.buffer()
        issued, pid, session_key = open_internal_ticket(self.server_key, ticket)
        if time.time() - issued > self.cfg.get("ticket_lifetime", 120) + 300:
            raise ValueError("ticket expired")
        r = StreamIn(kerberos_decrypt(session_key, req))
        rpid, cid, check = r.pid(), r.u32(), r.u32()
        if rpid != pid:
            raise ValueError("pid mismatch in connection request")
        conn.pid, conn.cid = pid, cid
        conn.set_session_key(session_key)
        return StreamOut().buffer(StreamOut().u32((check + 1) & 0xFFFFFFFF).bytes()).bytes()


# --------------------------------------------------------------------------- secure
def _order_param(s):
    return {"order_calc": s.u8(), "group_index": s.u8(), "group_num": s.u8(),
            "time_scope": s.u8(), "offset": s.u32(), "length": s.u8()}


class Secure:
    def __init__(self, cfg, store):
        self.cfg, self.store = cfg, store

    # SecureConnection
    def _register(self, conn, urls, out):
        public = None
        for u in urls:
            scheme, p = parse_station_url(u)
            if scheme:
                public = dict(p); break
        public = public or {}
        public.update(address=conn.addr[0], port=conn.addr[1], natf=0, natm=0, type=3,
                      PID=conn.pid, RVCID=conn.connection_id)
        conn.ctx["urls"] = urls
        out.result(SUCCESS).u32(conn.connection_id).string(station_url("prudp", **public))

    def register(self, conn, s, out):                     # 1
        self._register(conn, s.list(lambda x: x.string()), out)

    def request_connection_data(self, conn, s, out):      # 2
        s.u32(); s.u32()
        out.bool(False).u32(0)

    def request_urls(self, conn, s, out):                 # 3
        s.u32(); s.u32()
        out.bool(False).u32(0)

    def register_ex(self, conn, s, out):                  # 4
        urls = s.list(lambda x: x.string())
        try:
            name, data = s.anydata()
            conn.ctx["token"] = StreamIn(data).string()
        except Exception:
            pass
        self._register(conn, urls, out)

    def test_connectivity(self, conn, s, out):            # 5
        pass

    # Utility
    def acquire_unique_id(self, conn, s, out):            # 1
        out.u64(self.store.new_unique_id(conn.pid))

    # Ranking
    def upload_score(self, conn, s, out):                 # 1
        category, score, order_by, update_mode = s.u32(), s.u32(), s.u8(), s.u8()
        groups, param = s.qvector_u8(), s.u64()
        uid = s.u64()
        self.store.upload_score(conn.pid, uid, category, score, order_by, update_mode, groups, param)
        log.info("score pid=%d uid=%x category=%d score=%d groups=%s", conn.pid, uid, category, score, groups.hex())

    def delete_score(self, conn, s, out):                 # 2
        category, uid = s.u32(), s.u64()
        self.store.delete_score(conn.pid, uid, category)

    def delete_all_scores(self, conn, s, out):            # 3
        self.store.delete_all(conn.pid, s.u64())

    def upload_common_data(self, conn, s, out):           # 4
        data, uid = s.qvector_u8(), s.u64()
        if len(data) > 255:
            raise RMCError("Ranking::InvalidDataSize")
        self.store.set_common(conn.pid, uid, data)

    def delete_common_data(self, conn, s, out):           # 5
        self.store.delete_common(conn.pid, s.u64())

    def get_common_data(self, conn, s, out):              # 6
        data = self.store.get_common(conn.pid, s.u64())
        if data is None:
            raise RMCError("Ranking::NotFound")
        out.qvector_u8(data)

    def change_attributes(self, conn, s, out):            # 7
        category = s.u32()
        flag, groups, param = s.u8(), s.qvector_u8(), s.u64()
        self.store.change_attributes(conn.pid, s.u64(), category, flag, groups, param)

    def change_all_attributes(self, conn, s, out):        # 8
        flag, groups, param = s.u8(), s.qvector_u8(), s.u64()
        self.store.change_attributes(conn.pid, s.u64(), None, flag, groups, param)

    def _result(self, out, entries, total):
        # CONFIRMED ROOT CAUSE of a null-pointer crash in Wii Karaoke U:
        # raising Ranking::NotFound for an empty ranking list (e.g. a brand
        # new song nobody has scored on yet) made the client receive an
        # RMC error instead of a success reply, and its own error-handling
        # path for GetRanking doesn't check for null before dereferencing
        # whatever it expected to get back - crashing immediately after the
        # NEX round-trip (confirmed via WiiUCrashLogDumper: SRR0 inside
        # app.rpx's own ranking-state-machine code, r3=0 from a "find in
        # collection" helper whose collection count was 0). "No scores yet"
        # is a completely normal state for a ranking query, unlike
        # get_common_data's NotFound (a specific (pid, unique_id) record
        # either exists or doesn't) - a success reply with 0 entries is the
        # correct response here, not an error.
        out.u32(len(entries))
        for e in entries:
            # EDUCATED GUESS, not yet hardware-confirmed: the self-placeholder
            # fix above made GetRanking's own entry list non-empty, but the
            # exact same crash site still fires for other songs - the next
            # most likely empty collection is each entry's own "common data"
            # qvector, which is also always empty (common_data table has
            # never been written to by anything). If the client parses this
            # per-entry blob into its own fixed-record local collection the
            # same way it does the entries list, an empty one would hit the
            # identical null-collection pattern. 60 zero bytes mirrors the
            # element size (60) the crashing search helper was called with.
            common = self.store.get_common(e.pid, e.unique_id) or (b"\0" * 60)
            out.pid(e.pid).u64(e.unique_id).u32(e.order).u32(e.category).u32(e.score)
            out.qvector_u8(e.groups).u64(e.param or 0).qvector_u8(common)
        out.u32(total).datetime(nex_datetime())

    def get_ranking(self, conn, s, out):                  # 9
        mode, category, op = s.u8(), s.u32(), None
        op = _order_param(s)
        uid, pid = s.u64(), s.pid()
        want_pid = pid or conn.pid
        ents, total = self.store.ranking(mode, category, op, want_pid, uid)
        # SECOND crash found via the same WiiUCrashLogDumper trace, one step
        # further into the same state machine (the empty-result fix above
        # resolved the FIRST null, then the client advanced and crashed
        # again): it also does a local "find my own entry" lookup in
        # whatever list it just received and doesn't null-check that
        # either. mode=0 is a plain range query, not "around self", so a
        # song nobody (including the requesting player) has ever scored on
        # legitimately has no "self" entry - but the client still expects
        # to find one. Ensure it always can, with a synthetic 0-score
        # placeholder, rather than chasing every possible query shape that
        # could hit this.
        if not any(e.pid == want_pid and (uid == 0 or e.unique_id == uid) for e in ents):
            placeholder = Entry((want_pid, uid, category, 0, 1, b"", 0, time.time()))
            placeholder.order = total + 1
            ents = ents + [placeholder]
            total += 1
        log.info("GetRanking mode=%d category=%d %s -> %d/%d", mode, category, op, len(ents), total)
        self._result(out, ents, total)

    def get_approx_order(self, conn, s, out):             # 10
        category = s.u32(); op = _order_param(s); score = s.u32()
        s.u64(); s.pid()
        out.u32(self.store.approx_order(category, op, score))

    def get_stats(self, conn, s, out):                    # 11
        category = s.u32(); op = _order_param(s); flags = s.u32()
        vals = self.store.stats(category, op, flags)
        out.u32(len(vals))
        for v in vals:
            out.f64(v)

    def get_ranking_by_pid_list(self, conn, s, out):      # 12
        pids = s.list(lambda x: x.pid())
        mode, category = s.u8(), s.u32(); op = _order_param(s); uid = s.u64()
        ents, total = self.store.by_pids(set(pids), category, op, uid)
        self._result(out, ents, total)

    def get_ranking_by_unique_id_list(self, conn, s, out):  # 13
        uids = s.list(lambda x: x.u64())
        mode, category = s.u8(), s.u32(); op = _order_param(s); s.u64()
        ents, total = self.store.by_unique_ids(set(uids), category, op)
        self._result(out, ents, total)

    # stubs
    def ping_true(self, conn, s, out): out.bool(True)
    def cluster(self, conn, s, out): out.u32(0)
    def remote_log(self, conn, s, out): log.info("RemoteLog pid=%s: %s", conn.pid, s.string())

    def table(self):
        return {
            11: {1: self.register, 2: self.request_connection_data, 3: self.request_urls,
                 4: self.register_ex, 5: self.test_connectivity},
            110: {1: self.acquire_unique_id},
            112: {1: self.upload_score, 2: self.delete_score, 3: self.delete_all_scores,
                  4: self.upload_common_data, 5: self.delete_common_data, 6: self.get_common_data,
                  7: self.change_attributes, 8: self.change_all_attributes, 9: self.get_ranking,
                  10: self.get_approx_order, 11: self.get_stats, 12: self.get_ranking_by_pid_list,
                  13: self.get_ranking_by_unique_id_list},
            18: {1: self.ping_true, 2: self.ping_true, 3: self.ping_true, 4: self.ping_true},
            19: {1: self.ping_true, 2: self.cluster},
            1: {1: self.remote_log},
        }
