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

func (c *samuraiCatalog) appByTitleID(tid string) *samuraiApp {
	for i := range c.Apps {
		if strings.EqualFold(c.Apps[i].TitleID, tid) {
			return &c.Apps[i]
		}
	}
	return nil
}

func samuraiAppsDirectory(cat *samuraiCatalog) map[string]any {
	icon, banner := "", ""
	if len(cat.Apps) > 0 {
		icon, banner = cat.Apps[0].Icon, cat.Apps[0].Banner
	}
	return map[string]any{
		"id": samuraiAppsDirID, "type": "normal", "standard": false, "new": true,
		"name": "Apps", "description": "Homebrew from the Revivetendo community",
		"icon_url": samuraiMediaURL(icon), "icon_width": 102, "icon_height": 76,
		"banner_url": samuraiMediaURL(banner),
	}
}

func samuraiAppsDirectoryContents(cat *samuraiCatalog) map[string]any {
	d := samuraiAppsDirectory(cat)
	contents := []any{}
	for i := range cat.Apps {
		contents = append(contents, map[string]any{"index": len(contents) + 1, "title": samuraiTitleJSON(&cat.Apps[i], false)})
	}
	d["component"] = "title"
	d["contents"] = map[string]any{"content": contents, "length": len(contents), "offset": 0, "total": len(contents)}
	return map[string]any{"directory": d}
}

// samuraiTitleJSON is an app as a samurai title, the short form for lists or
// the full title page.
func samuraiTitleJSON(a *samuraiApp, full bool) map[string]any {
	m := map[string]any{
		"id": a.NSUID, "public": true, "new": a.New,
		"product_code": "CTR-N-" + strings.ToUpper(a.TitleID[8:12]),
		"name":         a.Name,
		"icon_url":     samuraiMediaURL(a.Icon),
		"platform":     map[string]any{"id": 124, "device": "CTR", "category": 2, "name": "Nintendo 3DS Download Software"},
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
		for _, tid := range q["title_id[]"] {
			if a := cat.appByTitleID(tid); a != nil {
				pairs = append(pairs, map[string]any{"title_id": strings.ToUpper(a.TitleID), "ns_uid": a.NSUID, "type": "T"})
			}
		}
		if len(pairs) == 0 {
			return false
		}
		log.Printf("eshop apps: id_pair %v -> %d", q["title_id[]"], len(pairs))
		fmt.Fprintf(w, `{"title_id_pairs":{"title_id_pair":%s}}`, mustJSON(pairs))
		return true

	case ninjaECInfoRe.MatchString(path):
		infos := []any{}
		for _, s := range q["ns_uid[]"] {
			id, _ := strconv.ParseInt(s, 10, 64)
			a := cat.appByNSUID(id)
			if a == nil {
				continue
			}
			ci, err := loadAppCIA(a)
			if err != nil {
				log.Printf("eshop apps: ec_info %d: %v", id, err)
				continue
			}
			infos = append(infos, map[string]any{"title_id": strings.ToUpper(a.TitleID), "title_version": ci.version, "content_size": ci.contentSize()})
		}
		if len(infos) == 0 {
			return false
		}
		log.Printf("eshop apps: ec_info %v from %s", q["ns_uid[]"], ip)
		fmt.Fprintf(w, `{"title_ec_infos":{"title_ec_info":%s}}`, mustJSON(infos))
		return true

	case ninjaOnlinePriceRe.MatchString(path):
		var entries []string
		for _, s := range append(q["title[]"], q["title"]...) {
			id, _ := strconv.ParseInt(s, 10, 64)
			if a := cat.appByNSUID(id); a != nil {
				entries = append(entries, fmt.Sprintf(`{"title_id":%d,"eshop_sales_status":"onsale","price":{"regular_price":{"id":%d,"amount":"%d,00 %s","currency":"ECOIN","raw_value":"%d"}}}`,
					a.NSUID, a.NSUID, a.Price, ninjaCoinName, a.Price))
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
		a := cat.appByNSUID(id)
		if a == nil {
			return false
		}
		size := int64(0)
		if ci, err := loadAppCIA(a); err == nil {
			size = ci.contentSize()
		}
		log.Printf("eshop apps: prepurchase_info %d (%s) from %s", id, a.Name, ip)
		fmt.Fprintf(w, `{"prepurchase_info":{"tax_excluded":false,"total_amount":%s,"purchasing_content":[{"eshop_sales_status":"onsale","content_size":%d,"payment_amount":{"total_amount":%s,"price":{"regular_price":{"id":"%d"}}}}]}}`,
			ninjaPriceJSON(a.Price), size, ninjaPriceJSON(a.Price), a.NSUID)
		return true

	case r.Method == http.MethodPost && ninjaAppPurchaseRe.MatchString(path):
		id, _ := strconv.ParseInt(ninjaAppPurchaseRe.FindStringSubmatch(path)[1], 10, 64)
		a := cat.appByNSUID(id)
		if a == nil {
			return false
		}
		bal := ninjaCoinDaily
		if pid := fetchRealPID(r); pid != 0 {
			var ok bool
			if a.Price > 0 {
				bal, ok = walletSpend(pid, a.Price)
				if !ok {
					log.Printf("eshop apps: purchase of %s REFUSED for PID %d: %d coins needed, balance %d", a.Name, pid, a.Price, bal)
					w.WriteHeader(http.StatusBadRequest)
					w.Write([]byte(`{"error":{"code":"3001","message":"insufficient balance"}}`))
					return true
				}
			} else {
				bal = walletBalance(pid)
			}
			log.Printf("eshop apps: PID %d bought %s for %d coins, balance %d", pid, a.Name, a.Price, bal)
		}
		ticket := time.Now().UnixNano() / 1000
		ninjaTicketTitle.Store(ticket, a.TitleID)
		log.Printf("eshop apps: !purchase %d (%s) from %s form=%v -> ticket %d", id, a.Name, ip, postForm, ticket)
		fmt.Fprintf(w, `{"transaction_result":{"ticket_id":%d,"post_balance":%s}}`, ticket, ninjaBalanceJSON(bal))
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

func appCIAPath(a *samuraiApp) string {
	return filepath.Join(samuraiDir, "apps", filepath.Base(a.CIA))
}

func loadAppCIA(a *samuraiApp) (*appCIA, error) {
	p := appCIAPath(a)
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
	var a *samuraiApp
	if m != nil && err == nil {
		a = cat.appByTitleID(m[1])
	}
	if a == nil {
		log.Printf("eshop apps: CCS %s %s (range %q) from %s -> 404", r.Method, r.URL.RequestURI(), r.Header.Get("Range"), ip)
		http.NotFound(w, r)
		return
	}
	ci, err := loadAppCIA(a)
	if err != nil {
		log.Printf("eshop apps: CCS %s: %v", r.URL.Path, err)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
		return
	}
	var data []byte
	if strings.HasPrefix(m[2], "tmd") {
		data = ci.tmd
	} else {
		id, _ := strconv.ParseUint(m[2], 16, 32)
		data = ci.contents[uint32(id)]
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

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
