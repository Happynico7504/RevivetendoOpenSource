package main

// Homebrew titles in the 3DS eShop (proof of concept, 2026-10-12): mint lists
// them on an "Apps" channel, buys them through ninja, nim fetches the ticket
// from our ECS and then downloads the title from nus.c.shop.nicoch.net/ccs/.
//
// It only works because every Revivetendo console runs Luma3DS, which patches
// out the signature checks: our tickets and the makerom TMDs are unsigned.
// Contents are not encrypted (their TMD content type has no encrypted bit), so
// the title key in our tickets (zero) is never used.
//
// Each app is a CIA in eshop-video/apps/ listed in catalog.json "apps":
//
//	{"ns_uid": 50010099000001, "title_id": "000400000E5ED100", "name": "...",
//	 "description": "...", "publisher": "Revivetendo", "icon": "i/...",
//	 "banner": "i/...", "price": 0, "cia": "rvtshoptest.cia", "new": true}
//
// Response shapes: ec_info and !purchase from mint's own parsers (decompiled,
// FUN_0015eef0 and FUN_001ea398); the title page from an archived real
// samurai title (XML, attributes as fields like the rest of samurai.go);
// online_prices and the CCS paths are best guesses and log what they serve.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type samuraiApp struct {
	NSUID       int64  `json:"ns_uid"`
	TitleID     string `json:"title_id"` // 16 hex digits
	Name        string `json:"name"`
	Description string `json:"description"`
	Publisher   string `json:"publisher"`
	Icon        string `json:"icon"`   // media path
	Banner      string `json:"banner"` // media path, 400x168
	Price       int    `json:"price"`  // RevivetendoCoins, 0 = free
	CIA         string `json:"cia"`    // file in eshop-video/apps/
	New         bool   `json:"new"`
	OwnerPID    uint32 `json:"owner_pid"` // seller: paid purchases are credited to this wallet; 0 = Revivetendo
	// Earlier versions' CIAs (files in eshop-video/apps/), kept so the eShop
	// can still fetch an installed older version's TMD after an update.
	OldCIAs []string `json:"old_cias"`
	// Update title (0004000E + the app's unique id), installed next to the
	// base and used instead of it - how 3DS updates work, so the version list
	// and the HOME Menu treat it as an update. Its own store id, always free.
	Update *struct {
		NSUID   int64    `json:"ns_uid"`
		CIA     string   `json:"cia"`
		OldCIAs []string `json:"old_cias"`
	} `json:"update"`
}

// shopTitle is one installable title the eShop deals with: an app's base
// title or its update title.
type shopTitle struct {
	App      *samuraiApp
	NSUID    int64
	TitleID  string // 16 upper-case hex digits
	CIA      string
	OldCIAs  []string
	Price    int
	IsUpdate bool
	Version  uint16 // TMD title version of CIA
	// For an older update version: the current one, which it points to.
	Newest *shopTitle
}

// versionString is a TMD title version as the 3DS shows it (major.minor.micro).
func versionString(v uint16) string {
	return fmt.Sprintf("%d.%d.%d", v>>10, (v>>4)&0x3f, v&0xf)
}

func ciaVersion(name string) uint16 {
	if ci, err := loadAppCIAFile(name); err == nil {
		return ci.version
	}
	return 0
}

func (a *samuraiApp) baseTitle() *shopTitle {
	return &shopTitle{App: a, NSUID: a.NSUID, TitleID: strings.ToUpper(a.TitleID), CIA: a.CIA, OldCIAs: a.OldCIAs,
		Price: a.Price, Version: ciaVersion(a.CIA)}
}

// updateTitles lists an app's update versions, newest first. Every version is
// a shop listing of its own: the eShop refuses an update whose listing the
// player already owns ("you already have this"), so a newer version needs a
// new ns_uid - the configured one plus the TMD version x 1000 (unique per
// version and per app). Published entries are never changed: older versions
// stay listed exactly as they were; Newest only marks them for the pointers.
func (a *samuraiApp) updateTitles() []*shopTitle {
	if a.Update == nil || len(a.TitleID) != 16 {
		return nil
	}
	tid := "0004000E" + strings.ToUpper(a.TitleID[8:])
	files := []string{a.Update.CIA}
	for i := len(a.Update.OldCIAs) - 1; i >= 0; i-- {
		files = append(files, a.Update.OldCIAs[i])
	}
	var out []*shopTitle
	for i, f := range files {
		v := ciaVersion(f)
		var others []string
		for j, g := range files {
			if j != i {
				others = append(others, g)
			}
		}
		t := &shopTitle{App: a, NSUID: a.Update.NSUID + int64(v)*1000, TitleID: tid, CIA: f, OldCIAs: others, IsUpdate: true, Version: v}
		if i > 0 {
			t.Newest = out[0]
		}
		out = append(out, t)
	}
	return out
}

// shopTitles lists every base and update title in the catalog.
func (c *samuraiCatalog) shopTitles() []*shopTitle {
	var out []*shopTitle
	for i := range c.Apps {
		out = append(out, c.Apps[i].baseTitle())
		out = append(out, c.Apps[i].updateTitles()...)
	}
	return out
}

func (c *samuraiCatalog) titleByNSUID(id int64) *shopTitle {
	for _, t := range c.shopTitles() {
		if t.NSUID == id {
			return t
		}
	}
	return nil
}

func (c *samuraiCatalog) titleByTitleID(tid string) *shopTitle {
	for _, t := range c.shopTitles() {
		if strings.EqualFold(t.TitleID, tid) {
			return t
		}
	}
	return nil
}

func (t *shopTitle) name() string {
	// A published update entry never changes: its name carries its own
	// version from the start, so a newer version is simply another entry.
	if t.IsUpdate {
		return t.App.Name + " Update (v" + versionString(t.Version) + ")"
	}
	return t.App.Name
}

// --- owned tickets ---
//
// Every app purchase is kept in eshop_app_tickets: the ticket id we returned
// from !purchase, the title, the buyer's PID, and - once nim fetches the
// ticket with AccountGetETickets - the console's ECS AccountId and DeviceId.
// ECS then lists them as the account's owned tickets (<TIV>ticket.version</TIV>
// in GetAccountStatus and AccountListETicketIds). Without that, a download the
// player moved to sleep mode never continued: the background task checked in
// with GetAccountStatus three times and gave up (2026-10-12). The table also
// lets AccountGetETickets resolve tickets after an account-proxy restart (the
// ninjaTicketTitle map is in memory only).

const appTicketSchema = `CREATE TABLE IF NOT EXISTS eshop_app_tickets (
	ticket_id   BIGINT      PRIMARY KEY,
	title_id    TEXT        NOT NULL,
	pid         BIGINT      NOT NULL DEFAULT 0,
	account_id  TEXT        NOT NULL DEFAULT '',
	device_id   TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS eshop_app_tickets_account ON eshop_app_tickets (account_id);
CREATE INDEX IF NOT EXISTS eshop_app_tickets_device ON eshop_app_tickets (device_id);`

var appTicketSchemaOnce sync.Once

func appTicketsReady() {
	appTicketSchemaOnce.Do(func() {
		if _, err := db.Exec(appTicketSchema); err != nil {
			log.Printf("eshop apps: ticket table: %v", err)
		}
	})
}

func appTicketRecordPurchase(ticket int64, titleID string, pid uint32) {
	appTicketsReady()
	if _, err := db.Exec(`INSERT INTO eshop_app_tickets (ticket_id, title_id, pid) VALUES ($1, $2, $3) ON CONFLICT (ticket_id) DO NOTHING`,
		ticket, strings.ToUpper(titleID), int64(pid)); err != nil {
		log.Printf("eshop apps: recording ticket %d: %v", ticket, err)
	}
}

// appTicketTitle returns the title an app ticket was bought for, binding the
// ticket to the console's ECS account and device on the way.
func appTicketTitle(ticket int64, accountID, deviceID string) (string, bool) {
	appTicketsReady()
	var title string
	if db.QueryRow(`UPDATE eshop_app_tickets SET account_id = $2, device_id = $3 WHERE ticket_id = $1 RETURNING title_id`,
		ticket, accountID, deviceID).Scan(&title) != nil {
		return "", false
	}
	return title, true
}

// appTicketTitlesForPID lists the distinct titles a player bought from us,
// oldest purchase first.
func appTicketTitlesForPID(pid uint32) []string {
	appTicketsReady()
	rows, err := db.Query(`SELECT title_id FROM eshop_app_tickets WHERE pid = $1 GROUP BY title_id ORDER BY MIN(created_at)`, int64(pid))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			out = append(out, t)
		}
	}
	return out
}

type appPurchase struct {
	Ticket  int64
	TitleID string
	Created time.Time
}

// appTicketPurchasesForPID lists a player's app and update purchases, newest
// first.
func appTicketPurchasesForPID(pid uint32) []appPurchase {
	appTicketsReady()
	rows, err := db.Query(`SELECT ticket_id, title_id, created_at FROM eshop_app_tickets WHERE pid = $1 ORDER BY created_at DESC LIMIT 200`, int64(pid))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []appPurchase
	for rows.Next() {
		var p appPurchase
		if rows.Scan(&p.Ticket, &p.TitleID, &p.Created) == nil {
			out = append(out, p)
		}
	}
	return out
}

// appTicketTIVs lists the app tickets owned by this ECS account or device as
// <TIV> elements (our tickets are version 0).
func appTicketTIVs(accountID, deviceID string) string {
	if accountID == "" && deviceID == "" {
		return ""
	}
	appTicketsReady()
	rows, err := db.Query(`SELECT ticket_id FROM eshop_app_tickets
		WHERE (account_id <> '' AND account_id = $1) OR (device_id <> '' AND device_id = $2) ORDER BY ticket_id`, accountID, deviceID)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var t int64
		if rows.Scan(&t) == nil {
			fmt.Fprintf(&sb, "<TIV>%d.0</TIV>", t)
		}
	}
	return sb.String()
}

// samuraiAppsDirID is the directory (channel) mint shows the apps in.
const samuraiAppsDirID = 9000

func (c *samuraiCatalog) appByNSUID(id int64) *samuraiApp {
	for i := range c.Apps {
		if c.Apps[i].NSUID == id {
			return &c.Apps[i]
		}
	}
	return nil
}

// samuraiAppsDirectory is the Apps channel. Its artwork is our own
// (custom-tools/eshop-art/make_art.py, same style as Revivetendo TV in blue).
func samuraiAppsDirectory(cat *samuraiCatalog) map[string]any {
	return map[string]any{
		"id": samuraiAppsDirID, "type": "normal", "standard": false, "new": true,
		"name": "Apps", "description": "Homebrew from the Revivetendo community",
		"icon_url": samuraiMediaURL("i/apps-channel-icon.jpg"), "icon_width": 102, "icon_height": 76,
		"banner_url": samuraiMediaURL("i/apps-channel-banner.jpg"),
	}
}

func samuraiAppsDirectoryContents(cat *samuraiCatalog) map[string]any {
	d := samuraiAppsDirectory(cat)
	contents := []any{}
	// Updates are shop entries of their own, right after their app, like on
	// Nintendo's eShop: that is where a player gets a newer update for an app
	// that already has one installed (the eShop only offers an update by
	// itself after downloading an app without one).
	for _, t := range cat.shopTitles() {
		contents = append(contents, map[string]any{"index": len(contents) + 1, "title": samuraiShopTitleJSON(t, false)})
	}
	d["component"] = "title"
	d["contents"] = map[string]any{"content": contents, "length": len(contents), "offset": 0, "total": len(contents)}
	return map[string]any{"directory": d}
}

// samuraiTitleJSON is an app as a samurai title, the short form for lists or
// the full title page.
// samuraiShopTitleJSON is samuraiTitleJSON for a base or update title: an
// update is shown as its app with its own id and "(update)" in the name.
func samuraiShopTitleJSON(t *shopTitle, full bool) map[string]any {
	m := samuraiTitleJSON(t.App, full)
	// tiger's title page (FUN_002409dc) reads which title and version a
	// listing stands for.
	m["title_id"] = t.TitleID
	m["title_version"] = t.Version
	// An app with an update: its listing carries the update title and its
	// newest version, so the page can compare it with the installed update
	// and offer it (experiment 2026-10-13; the base download itself goes by
	// ec_info/CAS, which keep the base title).
	if !t.IsUpdate {
		if ups := t.App.updateTitles(); len(ups) > 0 {
			m["title_id"] = ups[0].TitleID
			m["title_version"] = ups[0].Version
		}
	}
	if t.IsUpdate {
		m["id"] = t.NSUID
		m["name"] = t.name()
		// Only this version's own data, so the entry stays exactly as it was
		// published; which version is current is said by the pointers (id_pair,
		// the version list, the app's own listing), not by editing old entries.
		m["new"] = false
		if full {
			m["formal_name"] = t.name()
			m["description"] = "Update data for " + t.App.Name + ", version " + versionString(t.Version) + "."
		}
	}
	return m
}

func samuraiTitleJSON(a *samuraiApp, full bool) map[string]any {
	m := map[string]any{
		"id": a.NSUID, "public": true, "new": a.New,
		"product_code": "CTR-N-" + strings.ToUpper(a.TitleID[8:12]),
		"name":         a.Name,
		"icon_url":     samuraiMediaURL(a.Icon),
		// 19 = "Nintendo 3DS (Download)" on archived samurai title pages (124
		// is Wii U download software).
		"platform":     map[string]any{"id": 19, "device": "CTR", "name": "Nintendo 3DS (Download)"},
		"publisher":    map[string]any{"id": 1, "name": a.Publisher},
		"retail_sales": false, "eshop_sales": true,
		"release_date_on_eshop": "2026-10-12",
	}
	if !full {
		return m
	}
	for k, v := range map[string]any{
		"formal_name":       a.Name,
		"banner_url":        samuraiMediaURL(a.Banner),
		"display_genre":     "Homebrew",
		"description":       a.Description,
		"genres":            map[string]any{"genre": []any{map[string]any{"id": 95, "name": "Homebrew"}}, "length": 1},
		"languages":         map[string]any{"language": []any{map[string]any{"iso_code": "en", "name": "English"}}},
		"number_of_players": "1 player",
		"demo_available":    false, "aoc_available": false, "in_app_purchase": false,
		"ticket_available": false, "download_code_sales": false,
		"download_card_sales": map[string]any{"available": false},
		"rating_info":         map[string]any{"display": false},
		"star_rating_info":    map[string]any{"score": 0, "votes": 0},
		"top_image":           map[string]any{"type": "banner", "url": samuraiMediaURL(a.Banner)},
		"copyright":           map[string]any{"text": "Homebrew. Not affiliated with Nintendo."},
	} {
		m[k] = v
	}
	return m
}

// --- ninja ---

var (
	ninjaAppPurchaseRe = regexp.MustCompile(`^/ninja/ws/[^/]*/title/(\d+)/!purchase$`)
	ninjaAppPrepurchRe = regexp.MustCompile(`^/ninja/ws/[^/]*/title/(\d+)/prepurchase_info$`)
	ninjaECInfoRe      = regexp.MustCompile(`^/ninja/ws/[^/]*/titles/ec_info$`)
	ninjaTitleECInfoRe = regexp.MustCompile(`^/ninja/ws/[^/]*/title/(\d+)/ec_info$`)
	ninjaOnlinePriceRe = regexp.MustCompile(`^/ninja/ws/[^/]*/titles/online_prices$`)
)

func ninjaPriceJSON(coins int) string {
	return fmt.Sprintf(`{"amount":"%d,00 %s","currency":"ECOIN","raw_value":"%d"}`, coins, ninjaCoinName, coins)
}

// handleNinjaApps answers the ninja calls about our apps and reports whether it
// did. Calls about anything else (Badge Arcade) fall through.
func handleNinjaApps(w http.ResponseWriter, r *http.Request, path string, postForm url.Values, ip string) bool {
	cat, err := loadSamuraiCatalog()
	if err != nil || len(cat.Apps) == 0 {
		return false
	}
	q := r.URL.Query()
	switch {
	case path == "/ninja/ws/titles/id_pair":
		pairs := []any{}
		// The Updates screen asks for every installed title's update id at
		// once, comma-separated in one title_id[] value.
		var tids []string
		for _, v := range q["title_id[]"] {
			tids = append(tids, strings.Split(v, ",")...)
		}
		for _, tid := range tids {
			if t := cat.titleByTitleID(strings.TrimSpace(tid)); t != nil {
				pairs = append(pairs, map[string]any{"title_id": t.TitleID, "ns_uid": t.NSUID, "type": "T"})
			}
		}
		if len(pairs) == 0 {
			return false
		}
		log.Printf("eshop apps: id_pair %v -> %d", q["title_id[]"], len(pairs))
		fmt.Fprintf(w, `{"title_id_pairs":{"title_id_pair":%s}}`, mustJSON(pairs))
		return true

	// The account's software (the eShop's redownload / update list), paged with
	// limit and offset. Shape from tiger's parser (FUN_00329500):
	// owned_titles{total, owned_title[{id: ns_uid, title_id: hex}]}; a missing
	// owned_titles is the generic 72001 (007-2001). Lists the base titles this
	// player bought from us; Badge Arcade has none here.
	// Account activity (purchase history): the apps and updates this player got
	// from us, newest first, paged. Shape from tiger's parsers (FUN_00313d58,
	// FUN_0031311c): transactions{total, offset, transaction[{index, id, date,
	// type, description, transaction_amount.amount, balance.amount, title.id,
	// traceable, receipt}]}. date uses the receipt's display format; the
	// balance after each purchase is not recorded, so it shows the current one.
	case path == "/ninja/ws/my/transactions":
		var entries []any
		pid := fetchRealPID(r)
		if pid != 0 {
			bal := ninjaBalanceJSON(walletBalance(pid))
			var balAmount struct {
				Amount string `json:"amount"`
			}
			json.Unmarshal([]byte(bal), &balAmount)
			for _, p := range appTicketPurchasesForPID(pid) {
				t := cat.titleByTitleID(p.TitleID)
				if t == nil {
					continue
				}
				price := t.Price
				if t.IsUpdate || pid == t.App.OwnerPID {
					price = 0
				}
				entries = append(entries, map[string]any{
					"index": len(entries) + 1, "id": p.Ticket,
					"date":               p.Created.Format("2006/01/02 15:04:05"),
					"type":               "title_purchase",
					"description":        t.name(),
					"transaction_amount": map[string]any{"amount": fmt.Sprintf("-%d,00 %s", price, ninjaCoinName)},
					"balance":            map[string]any{"amount": balAmount.Amount},
					"title":              map[string]any{"id": t.NSUID},
					"traceable":          false, "receipt": false,
				})
			}
		}
		total := len(entries)
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if offset < 0 || offset > total {
			offset = total
		}
		end := total
		if limit > 0 && offset+limit < total {
			end = offset + limit
		}
		page := []any{}
		if total > 0 {
			page = entries[offset:end]
		}
		log.Printf("eshop apps: transactions -> %d of %d for PID %d", len(page), total, pid)
		fmt.Fprintf(w, `{"transactions":{"total":%d,"offset":%d,"length":%d,"transaction":%s}}`, total, offset, len(page), mustJSON(page))
		return true

	// "Rate this software" upload after playing; we keep no ratings.
	case path == "/ninja/ws/my/votable_titles/!put":
		log.Printf("eshop apps: votable_titles/!put from %s form=%v", ip, postForm)
		w.Write([]byte(`{}`))
		return true

	case path == "/ninja/ws/my/shared_titles":
		owned := []any{}
		if pid := fetchRealPID(r); pid != 0 {
			for _, tid := range appTicketTitlesForPID(pid) {
				if t := cat.titleByTitleID(tid); t != nil && !t.IsUpdate {
					owned = append(owned, map[string]any{"id": t.NSUID, "title_id": t.TitleID})
				}
			}
		}
		total := len(owned)
		offset, _ := strconv.Atoi(q.Get("offset"))
		limit, _ := strconv.Atoi(q.Get("limit"))
		if offset < 0 || offset > total {
			offset = total
		}
		end := total
		if limit > 0 && offset+limit < total {
			end = offset + limit
		}
		log.Printf("eshop apps: shared_titles -> %d of %d from %s", end-offset, total, ip)
		fmt.Fprintf(w, `{"owned_titles":{"total":%d,"owned_title":%s}}`, total, mustJSON(owned[offset:end]))
		return true

	case ninjaECInfoRe.MatchString(path):
		infos := []any{}
		for _, s := range q["ns_uid[]"] {
			id, _ := strconv.ParseInt(s, 10, 64)
			t := cat.titleByNSUID(id)
			if t == nil {
				continue
			}
			ci, err := loadAppCIAFile(t.CIA)
			if err != nil {
				log.Printf("eshop apps: ec_info %d: %v", id, err)
				continue
			}
			infos = append(infos, map[string]any{"title_id": t.TitleID, "title_version": ci.version, "content_size": ci.contentSize()})
		}
		if len(infos) == 0 {
			return false
		}
		log.Printf("eshop apps: ec_info %v from %s", q["ns_uid[]"], ip)
		fmt.Fprintf(w, `{"title_ec_infos":{"title_ec_info":%s}}`, mustJSON(infos))
		return true

	// The title page asks for one title at a time. No parser for this one is in
	// code.bin (the page lives in a module we have not dumped): same fields as
	// titles/ec_info, unwrapped, plus disable_download. Unhandled, it was 011-2500.
	case ninjaTitleECInfoRe.MatchString(path):
		id, _ := strconv.ParseInt(ninjaTitleECInfoRe.FindStringSubmatch(path)[1], 10, 64)
		t := cat.titleByNSUID(id)
		if t == nil {
			return false
		}
		ci, err := loadAppCIAFile(t.CIA)
		if err != nil {
			log.Printf("eshop apps: ec_info %d: %v", id, err)
			return false
		}
		log.Printf("eshop apps: title ec_info %d (%s) from %s", id, t.name(), ip)
		fmt.Fprintf(w, `{"title_ec_info":{"title_id":"%s","title_version":%d,"content_size":%d,"disable_download":false}}`, t.TitleID, ci.version, ci.contentSize())
		return true

	case ninjaOnlinePriceRe.MatchString(path):
		var entries []string
		// The channel list asks for all its titles at once, comma-separated
		// in one title[] value; a title page asks for one.
		var ids []string
		for _, v := range append(q["title[]"], q["title"]...) {
			ids = append(ids, strings.Split(v, ",")...)
		}
		for _, s := range ids {
			id, _ := strconv.ParseInt(s, 10, 64)
			if t := cat.titleByNSUID(id); t != nil {
				entries = append(entries, fmt.Sprintf(`{"title_id":%d,"eshop_sales_status":"onsale","price":{"regular_price":{"id":%d,"amount":"%d,00 %s","currency":"ECOIN","raw_value":"%d"}}}`,
					t.NSUID, t.NSUID, t.Price, ninjaCoinName, t.Price))
			}
		}
		if len(entries) == 0 {
			return false
		}
		log.Printf("eshop apps: online_prices %s from %s", r.URL.RawQuery, ip)
		fmt.Fprintf(w, `{"online_prices":{"online_price":[%s]}}`, strings.Join(entries, ","))
		return true

	case ninjaAppPrepurchRe.MatchString(path):
		id, _ := strconv.ParseInt(ninjaAppPrepurchRe.FindStringSubmatch(path)[1], 10, 64)
		t := cat.titleByNSUID(id)
		if t == nil {
			return false
		}
		size := int64(0)
		if ci, err := loadAppCIAFile(t.CIA); err == nil {
			size = ci.contentSize()
		}
		// Fields as the eShop (tiger, FUN_00331ec0) reads them: purchasing_content
		// must hold exactly one entry whose payment_amount has total_amount.raw_value
		// and price.regular_price with a NUMERIC id and a raw_value - without them
		// the Download button failed with the generic 007-2001 (72001).
		// tax_amount and current_balance are optional.
		bal := ninjaCoinDaily
		if pid := fetchRealPID(r); pid != 0 {
			bal = walletBalance(pid)
		}
		price := ninjaPriceJSON(t.Price)
		log.Printf("eshop apps: prepurchase_info %d (%s) from %s", id, t.name(), ip)
		fmt.Fprintf(w, `{"prepurchase_info":{"tax_excluded":false,"current_balance":%s,"total_amount":%s,"purchasing_content":[{"eshop_sales_status":"onsale","content_size":%d,"payment_amount":{"total_amount":%s,"tax_amount":%s,"price":{"regular_price":{"id":%d,"amount":"%d,00 %s","currency":"ECOIN","raw_value":"%d"}}}}]}}`,
			ninjaBalanceJSON(bal), price, size, price, ninjaPriceJSON(0), t.NSUID, t.Price, ninjaCoinName, t.Price)
		return true

	case r.Method == http.MethodPost && ninjaAppPurchaseRe.MatchString(path):
		id, _ := strconv.ParseInt(ninjaAppPurchaseRe.FindStringSubmatch(path)[1], 10, 64)
		t := cat.titleByNSUID(id)
		if t == nil {
			return false
		}
		a := t.App
		bal := ninjaCoinDaily
		ticket := time.Now().UnixNano() / 1000
		if pid := fetchRealPID(r); pid != 0 {
			price := t.Price
			if pid == a.OwnerPID {
				price = 0 // sellers get their own titles for free
			}
			var ok bool
			if price > 0 {
				bal, ok = walletSpend(pid, price)
				if !ok {
					log.Printf("eshop apps: purchase of %s REFUSED for PID %d: %d coins needed, balance %d", a.Name, pid, price, bal)
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(`{"error":{"code":"3001","message":"insufficient balance"}}`))
					return true
				}
				// Earnings go to the seller in full and are not limited by the
				// 500-coin cap (that only stops the free daily grant). The ticket
				// id as ref makes the credit happen once per purchase.
				if a.OwnerPID != 0 {
					if sbal, applied, err := walletAdjust(a.OwnerPID, price, nil, "sale "+a.TitleID, fmt.Sprintf("sale:%d", ticket)); err != nil {
						log.Printf("eshop apps: crediting seller %d for %s: %v", a.OwnerPID, a.Name, err)
					} else if applied {
						log.Printf("eshop apps: seller %d earned %d coins for %s, balance %d", a.OwnerPID, price, a.Name, sbal)
					}
				}
			} else {
				bal = walletBalance(pid)
			}
			log.Printf("eshop apps: PID %d bought %s for %d coins, balance %d", pid, a.Name, price, bal)
		}
		ninjaTicketTitle.Store(ticket, t.TitleID)
		appTicketRecordPurchase(ticket, t.TitleID, fetchRealPID(r))
		log.Printf("eshop apps: !purchase %d (%s) from %s form=%v -> ticket %d", id, t.name(), ip, postForm, ticket)
		fmt.Fprintf(w, `{"transaction_result":{"transaction_id":"%d","ticket_id":%d,"post_balance":%s}}`, ticket, ticket, ninjaBalanceJSON(bal))
		return true
	}
	return false
}

// --- CCS (content download) ---

// appCIA is a CIA split into what nim downloads: the TMD and each content.
type appCIA struct {
	version  uint16
	tmd      []byte
	contents map[uint32][]byte // content id -> data
	mtime    time.Time
}

func (c *appCIA) contentSize() int64 {
	var n int64
	for _, d := range c.contents {
		n += int64(len(d))
	}
	return n
}

var appCIACache sync.Map // file path -> *appCIA

// loadAppCIAFile loads a CIA from eshop-video/apps/, cached until it changes.
func loadAppCIAFile(name string) (*appCIA, error) {
	p := filepath.Join(samuraiDir, "apps", filepath.Base(name))
	st, err := os.Stat(p)
	if err != nil {
		return nil, err
	}
	if v, ok := appCIACache.Load(p); ok && v.(*appCIA).mtime.Equal(st.ModTime()) {
		return v.(*appCIA), nil
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c, err := parseCIA(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	c.mtime = st.ModTime()
	appCIACache.Store(p, c)
	return c, nil
}

// parseCIA splits a CIA (layout per 3dbrew's CIA and Title metadata pages):
// header, certificate chain, ticket, TMD and contents, each 64-byte aligned.
func parseCIA(d []byte) (*appCIA, error) {
	if len(d) < 0x20 {
		return nil, fmt.Errorf("too short")
	}
	le := binary.LittleEndian
	hdr, certSz, tikSz, tmdSz := le.Uint32(d[0:]), le.Uint32(d[8:]), le.Uint32(d[12:]), le.Uint32(d[16:])
	align := func(x uint32) uint32 { return (x + 63) &^ 63 }
	tmdOff := align(hdr) + align(certSz) + align(tikSz)
	conOff := tmdOff + align(tmdSz)
	if int(conOff) > len(d) {
		return nil, fmt.Errorf("truncated")
	}
	tmd := d[tmdOff : tmdOff+tmdSz]
	be := binary.BigEndian
	sigLen := map[uint32]int{0x10003: 0x200 + 0x3C, 0x10004: 0x100 + 0x3C, 0x10005: 0x3C + 0x40}[be.Uint32(tmd)]
	if sigLen == 0 {
		return nil, fmt.Errorf("unknown TMD signature type %#x", be.Uint32(tmd))
	}
	h := tmd[4+sigLen:]
	c := &appCIA{version: be.Uint16(h[0x9C:]), tmd: tmd, contents: map[uint32][]byte{}}
	n := int(be.Uint16(h[0x9E:]))
	chunks := h[0xC4+0x900:]
	off := uint64(conOff)
	for i := 0; i < n; i++ {
		ch := chunks[i*0x30:]
		id, typ, size := be.Uint32(ch), be.Uint16(ch[6:]), be.Uint64(ch[8:])
		if typ&1 != 0 {
			return nil, fmt.Errorf("content %08x is encrypted", id)
		}
		if off+size > uint64(len(d)) {
			return nil, fmt.Errorf("content %08x truncated", id)
		}
		c.contents[id] = d[off : off+size]
		off = (off + size + 63) &^ 63
	}
	return c, nil
}

var ccsPathRe = regexp.MustCompile(`^/ccs/download/([0-9A-Fa-f]{16})/(tmd(?:\.\d+)?|[0-9A-Fa-f]{8})$`)

// serveCCS answers nim's content downloads:
// /ccs/download/<title id>/tmd[.<version>] and /ccs/download/<title id>/<content id>.
func serveCCS(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r)
	m := ccsPathRe.FindStringSubmatch(r.URL.Path)
	cat, err := loadSamuraiCatalog()
	var t *shopTitle
	if m != nil && err == nil {
		t = cat.titleByTitleID(m[1])
	}
	if t == nil {
		log.Printf("eshop apps: CCS %s %s (range %q) from %s -> 404", r.Method, r.URL.RequestURI(), r.Header.Get("Range"), ip)
		http.NotFound(w, r)
		return
	}
	ci, err := loadAppCIAFile(t.CIA)
	if err != nil {
		log.Printf("eshop apps: CCS %s: %v", r.URL.Path, err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	var data []byte
	if strings.HasPrefix(m[2], "tmd") {
		// Updates: "cia" is the newest version, "old_cias" the earlier ones.
		// "tmd" is the newest TMD; "tmd.<v>" answers with exactly that
		// version, like Nintendo's CDN (the eShop fetches the installed
		// version's TMD whenever a title page is opened).
		data = ci.tmd
		if v, ok := strings.CutPrefix(m[2], "tmd."); ok && v != strconv.Itoa(int(ci.version)) {
			data = nil
			for _, old := range t.OldCIAs {
				oc, err := loadAppCIAFile(old)
				if err == nil && strconv.Itoa(int(oc.version)) == v {
					data, ci = oc.tmd, oc
					break
				}
			}
			if data == nil {
				log.Printf("eshop apps: CCS %s from %s -> 404 (no v%s; newest is v%d)", r.URL.Path, ip, v, ci.version)
				http.NotFound(w, r)
				return
			}
		}
	} else {
		// Content ids differ per build, so an older version's content lives
		// only in its own CIA.
		id, _ := strconv.ParseUint(m[2], 16, 32)
		data = ci.contents[uint32(id)]
		for _, old := range t.OldCIAs {
			if data != nil {
				break
			}
			if oc, err := loadAppCIAFile(old); err == nil {
				if d, ok := oc.contents[uint32(id)]; ok {
					data, ci = d, oc
				}
			}
		}
	}
	if data == nil {
		log.Printf("eshop apps: CCS %s from %s -> no such content", r.URL.Path, ip)
		http.NotFound(w, r)
		return
	}
	log.Printf("eshop apps: CCS %s %s (range %q) from %s -> %d bytes", r.Method, r.URL.Path, r.Header.Get("Range"), ip, len(data))
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", ci.mtime, bytes.NewReader(data))
}

// serveTagayaVersionlist answers the title version list
// (https://tagaya-ctr.cdn.nintendo.net/tagaya/versionlist, which the Nimbus
// HTTP patch turns into *.pretendo.cc and so reaches us). It lists the newest
// version of every app, so installed older versions get the "update
// available" treatment. Layout from the eShop's parser (tiger FUN_00208b98):
// a 16-byte header whose first u16 must be 1, then 16-byte entries - u64
// title id, u32 version, u32 reserved, little-endian.
func serveTagayaVersionlist(w http.ResponseWriter, r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if !strings.HasPrefix(h, "tagaya-ctr.cdn.") || r.URL.Path != "/tagaya/versionlist" {
		return false
	}
	var buf bytes.Buffer
	hdr := make([]byte, 16)
	binary.LittleEndian.PutUint16(hdr, 1)
	buf.Write(hdr)
	n := 0
	if cat, err := loadSamuraiCatalog(); err == nil {
		for _, t := range cat.shopTitles() {
			if t.Newest != nil {
				continue // only the newest version of each title
			}
			tid, err := strconv.ParseUint(t.TitleID, 16, 64)
			ci, cerr := loadAppCIAFile(t.CIA)
			if err != nil || cerr != nil {
				continue
			}
			e := make([]byte, 16)
			binary.LittleEndian.PutUint64(e, tid)
			binary.LittleEndian.PutUint32(e[8:], uint32(ci.version))
			buf.Write(e)
			n++
		}
	}
	log.Printf("tagaya: versionlist (%d titles) for %s from %s", n, r.Host, realIP(r))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(buf.Bytes())
	return true
}

// serveNPNSNotifications answers nim's push-notification poll
// (https://<x>-npns.app.nintendo.net/api/v1/notifications.json, which the
// Nimbus patches turn into *.pretendo.cc) with an empty list. Pretendo does not
// answer it, so every sleep-mode wake-up logged an upstream failure; the
// experiment (2026-10-12) is whether that is what keeps nim from continuing a
// download moved to sleep mode. nim's parser (FUN_00118010) takes
// {"notifications":[...]} with at most maxlength entries.
func serveNPNSNotifications(w http.ResponseWriter, r *http.Request) bool {
	h := strings.ToLower(r.Host)
	if !strings.Contains(h, "-npns.app.") || !strings.HasSuffix(r.URL.Path, "/notifications.json") {
		return false
	}
	log.Printf("npns: %s %s from %s -> empty notification list", r.Method, r.URL.RequestURI(), realIP(r))
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"notifications":[]}`))
	return true
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var casTitleIDRe = regexp.MustCompile(`<cas:TitleId>([0-9A-Fa-f]{16})</cas:TitleId>`)

// casListTitlesEx answers mint's CAS ListTitlesEx (sent through nim) for our
// apps; ok is false when none of the asked titles is one. One <Titles> per
// title, like ListItems' <Items> (nim's strings pair them the same way), fields
// in nim's parse order: TitleId, Attributes, Prices (see the ListItems notes in
// main.go - Prices before Attributes is never found). The eShop (tiger,
// FUN_0022ae24) takes the first price. (mint, Badge Arcade's purchase applet,
// rejects a non-"0" <Amount> on its own free path - not the eShop.) It reads the
// Version and MaxUserFileSize attributes as numbers; MaxUserFileSize carries
// the content size (mint treats "0" as not downloadable).
func casListTitlesEx(body []byte, deviceID, messageID string) (string, bool) {
	cat, err := loadSamuraiCatalog()
	if err != nil {
		return "", false
	}
	var titles strings.Builder
	n := 0
	for _, m := range casTitleIDRe.FindAllStringSubmatch(string(body), -1) {
		t := cat.titleByTitleID(m[1])
		if t == nil {
			continue
		}
		version, size := 0, int64(0)
		if ci, err := loadAppCIAFile(t.CIA); err == nil {
			version, size = int(ci.version), ci.contentSize()
		}
		n++
		fmt.Fprintf(&titles, `<Titles><TitleId>%s</TitleId><Attributes><Name>Version</Name><Value>%d</Value></Attributes><Attributes><Name>MaxUserFileSize</Name><Value>%d</Value></Attributes><Prices><ItemId>%d</ItemId><Price><Amount>%d</Amount><Currency>EUR</Currency></Price><LicenseKind>PERMANENT</LicenseKind></Prices></Titles>`,
			t.TitleID, version, size, t.NSUID, t.Price)
	}
	if n == 0 {
		return "", false
	}
	log.Printf("eshop apps: CAS ListTitlesEx -> %d title(s)", n)
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?><soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/" xmlns:xsd="http://www.w3.org/2001/XMLSchema" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance"><soapenv:Body><ListTitlesExResponse xmlns="urn:cas.wsapi.broadon.com"><Version>2.0</Version><DeviceId>%s</DeviceId><MessageId>%s</MessageId><TimeStamp>%d</TimeStamp><ErrorCode>0</ErrorCode><ListResultTotalSize>%d</ListResultTotalSize>%s</ListTitlesExResponse></soapenv:Body></soapenv:Envelope>`,
		deviceID, messageID, time.Now().UnixMilli(), n, titles.String()), true
}
