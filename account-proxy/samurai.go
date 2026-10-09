package main

// --- Samurai (3DS eShop catalog) as a video platform ---
//
// The eShop app (mint) reads its whole catalog from samurai: telops (top-screen
// ticker), news, directories (the home tiles), directory/<id> (a tile's
// contents) and movie/<id> (a video page). Nobody runs a real 3DS eShop any
// more, so instead of games we list videos: the eShop already has a native
// "movie" content type (ids 5004...) that its own player plays as a 400x240
// Mobiclip .moflex file. Directories are channels, movies are videos.
//
// mint asks for `_type=json`. Response shapes follow ReShop-3ds's MIT-licensed
// Metadata server (github.com/ReShop-3ds/Metadata), which got real consoles
// into the eShop with JSON in this notation (XML attributes become plain
// fields, repeated elements become arrays). The movie fields come from
// archived real responses (web.archive.org, samurai.ctr.shop.nintendo.net
// .../movie/50040000049382?shop_id=1): name, banner_url (400x168),
// thumbnail_url (120x88), files.file[] {format, movie_url, width, height,
// dimension, play_time_sec}. Directory icons were 102x76 on the 3DS.
//
// The catalog lives in samuraiDir/catalog.json and is re-read on every
// request, so videos and channels can be added without a rebuild. Media files
// are served from samuraiDir/media at https://samurai.nicoch.net/media/....
//
// The Wii U eShop is different: it is a browser applet whose start page is
// https://ninja.wup.shop.nintendo.net/ninja/wood_index.html, which our Inkay
// fork rewrites to http://samurai.wup.shop.nicoch.net/ninja/wood_index.html
// (plain http, like upstream Inkay's allowlist entry; nginx proxies that port-80
// host here). Pretendo's own page there is just a wiiuErrorViewer
// "eShop is not available" script, so ours is an ordinary HTML page built from
// the same catalog, playing each video's .mp4 with <video>.

import (
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const samuraiDir = "/nico-pretendo-bridge/eshop-video"

type samuraiVideo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Banner      string `json:"banner"`    // media path, 400x168 jpg
	Thumbnail   string `json:"thumbnail"` // media path, 120x88 jpg
	File        string `json:"file"`      // media path, .moflex
	MP4         string `json:"mp4"`       // media path, H.264/AAC .mp4 for the Wii U page
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Dimension   string `json:"dimension"` // "2d" or "3d"
	Seconds     int    `json:"seconds"`
	New         bool   `json:"new"`
}

type samuraiChannel struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Description string  `json:"description"`
	Icon        string  `json:"icon"`   // media path, 102x76 jpg
	Banner      string  `json:"banner"` // media path, 400x168 jpg
	Videos      []int64 `json:"videos"`
	New         bool    `json:"new"`
}

type samuraiCatalog struct {
	Telops   []string         `json:"telops"`
	News     []samuraiNews    `json:"news"`
	About    string           `json:"about"`
	Channels []samuraiChannel `json:"channels"`
	Videos   []samuraiVideo   `json:"videos"`
}

type samuraiNews struct {
	ID       int64  `json:"id"`
	Headline string `json:"headline"`
	Body     string `json:"body"`
	Date     string `json:"date"` // 2006-01-02
}

func loadSamuraiCatalog() (*samuraiCatalog, error) {
	raw, err := os.ReadFile(filepath.Join(samuraiDir, "catalog.json"))
	if err != nil {
		return nil, err
	}
	var c samuraiCatalog
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (c *samuraiCatalog) video(id int64) *samuraiVideo {
	for i := range c.Videos {
		if c.Videos[i].ID == id {
			return &c.Videos[i]
		}
	}
	return nil
}

func samuraiMediaURL(p string) string {
	if p == "" {
		return ""
	}
	return "https://samurai.nicoch.net/media/" + strings.TrimPrefix(p, "/")
}

// samuraiMovieJSON is one movie as samurai listed it, in both directory
// contents and movie/<id>.
func samuraiMovieJSON(v *samuraiVideo) map[string]any {
	w, h, dim := v.Width, v.Height, v.Dimension
	if w == 0 {
		w = 400
	}
	if h == 0 {
		h = 240
	}
	if dim == "" {
		dim = "2d"
	}
	m := map[string]any{
		"id":            v.ID,
		"new":           v.New,
		"name":          v.Name,
		"banner_url":    samuraiMediaURL(v.Banner),
		"thumbnail_url": samuraiMediaURL(v.Thumbnail),
		"files": map[string]any{"file": []any{map[string]any{
			"format":        "moflex",
			"movie_url":     samuraiMediaURL(v.File),
			"width":         w,
			"height":        h,
			"dimension":     dim,
			"play_time_sec": v.Seconds,
		}}},
	}
	if v.Description != "" {
		m["description"] = v.Description
	}
	return m
}

func handleSamurai(w http.ResponseWriter, r *http.Request) {
	ip := realIP(r)
	if strings.HasPrefix(r.URL.Path, "/media/") {
		serveSamuraiMedia(w, r)
		return
	}
	log.Printf("samurai: %s %s from %s", r.Method, r.URL.RequestURI(), ip)

	// /samurai/ws/<country>/<endpoint...>
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 || parts[0] != "samurai" || parts[1] != "ws" {
		handleShopStub(w, r)
		return
	}
	endpoint := parts[3:]

	cat, err := loadSamuraiCatalog()
	if err != nil {
		log.Printf("samurai: catalog: %v", err)
		handleShopStub(w, r)
		return
	}

	var out any
	switch endpoint[0] {
	case "telops":
		telops := cat.Telops
		if telops == nil {
			telops = []string{}
		}
		out = map[string]any{"telops": map[string]any{"telop": telops, "length": len(telops)}}

	case "news":
		entries := []any{}
		for _, n := range cat.News {
			ts := time.Now().Unix()
			if t, err := time.Parse("2006-01-02", n.Date); err == nil {
				ts = t.Unix()
			}
			entries = append(entries, map[string]any{"id": n.ID, "headline": n.Headline, "description": n.Body, "date": ts})
		}
		out = map[string]any{"news": map[string]any{"news_entry": entries, "length": len(entries)}}

	case "languages":
		lang := r.URL.Query().Get("lang")
		if lang == "" {
			lang = "en"
		}
		out = map[string]any{"languages": map[string]any{"language": []any{map[string]any{"iso_code": lang, "name": "English"}}}}

	case "eshop_message":
		out = map[string]any{"text": map[string]any{"type": "html", "body": cat.About}}

	case "directories":
		dirs := []any{}
		for i, ch := range cat.Channels {
			dirs = append(dirs, map[string]any{
				"index":       i + 1,
				"id":          ch.ID,
				"type":        "normal",
				"standard":    false,
				"new":         ch.New,
				"name":        ch.Name,
				"icon_url":    samuraiMediaURL(ch.Icon),
				"icon_width":  102,
				"icon_height": 76,
				"banner_url":  samuraiMediaURL(ch.Banner),
				"description": ch.Description,
			})
		}
		out = map[string]any{"directories": map[string]any{"directory": dirs, "length": len(dirs), "catalog_id": 1}}

	case "directory":
		if len(endpoint) < 2 {
			break
		}
		id, _ := strconv.ParseInt(endpoint[1], 10, 64)
		for _, ch := range cat.Channels {
			if ch.ID != id {
				continue
			}
			contents := []any{}
			for _, vid := range ch.Videos {
				if v := cat.video(vid); v != nil {
					contents = append(contents, map[string]any{"index": len(contents) + 1, "movie": samuraiMovieJSON(v)})
				}
			}
			out = map[string]any{"directory": map[string]any{
				"id":          ch.ID,
				"name":        ch.Name,
				"type":        "normal",
				"component":   "movie",
				"icon_url":    samuraiMediaURL(ch.Icon),
				"icon_width":  102,
				"icon_height": 76,
				"banner_url":  samuraiMediaURL(ch.Banner),
				"description": ch.Description,
				"contents":    map[string]any{"content": contents, "length": len(contents), "offset": 0, "total": len(contents)},
			}}
		}

	case "movie":
		if len(endpoint) < 2 {
			break
		}
		id, _ := strconv.ParseInt(endpoint[1], 10, 64)
		if v := cat.video(id); v != nil {
			out = map[string]any{"movie": samuraiMovieJSON(v)}
		}

	case "movies", "contents":
		contents := []any{}
		for i := range cat.Videos {
			contents = append(contents, map[string]any{"index": len(contents) + 1, "movie": samuraiMovieJSON(&cat.Videos[i])})
		}
		out = map[string]any{"contents": map[string]any{"content": contents, "length": len(contents), "offset": 0, "total": len(contents)}}
	}

	if out == nil {
		handleShopStub(w, r)
		return
	}
	body, _ := json.Marshal(out)
	w.Header().Set("Content-Type", ninjaContentType(r))
	w.Write(body)
}

// wiiuShopPage is the Wii U eShop start page. The applet is an old WebKit
// (Safari 6 era), so the script sticks to ES5.
var wiiuShopPage = template.Must(template.New("wood").Parse(`<!DOCTYPE html>
<html><head><meta charset="utf-8"><title>Revivetendo TV</title>
<style>
body{margin:0;background:#141414;color:#fff;font-family:sans-serif}
#top{display:-webkit-box;-webkit-box-align:center;padding:16px 28px;background:#e2231a}
#top h1{-webkit-box-flex:1;margin:0;font-size:32px}
#close{font-size:22px;padding:10px 22px;border:0;border-radius:8px;background:#fff;color:#e2231a}
#telop{padding:10px 28px;background:#222;color:#ccc;font-size:18px}
.ch h2{margin:24px 28px 12px;font-size:26px}
.row{padding:0 28px}
.v{display:inline-block;vertical-align:top;width:260px;margin:0 18px 18px 0;background:#222;border-radius:10px;overflow:hidden}
.v img{display:block;width:260px;height:109px;background:#333}
.v p{margin:8px 12px 12px;font-size:18px}
#player{display:none;position:fixed;top:0;left:0;right:0;bottom:0;background:#000;text-align:center}
#player video{width:1280px;height:640px;max-width:100%;background:#000}
#player h2{margin:8px 0 0;font-size:24px}
#player button{position:absolute;top:12px;right:12px;font-size:22px;padding:10px 22px;border:0;border-radius:8px}
</style></head><body>
<div id="top"><h1>Revivetendo TV</h1><button id="close" onclick="closeApp()">Close</button></div>
{{range .Telops}}<div id="telop">{{.}}</div>{{end}}
{{range .Channels}}<div class="ch"><h2>{{.Name}}</h2><div class="row">
{{range .Videos}}<a class="v" href="#" onclick="play('{{.URL}}','{{.Name}}');return false"><img src="{{.Banner}}" alt=""><p>{{.Name}}</p></a>{{end}}
</div></div>{{end}}
<div id="player"><video id="vid" controls></video><h2 id="vname"></h2><button onclick="stop()">Back</button></div>
<script>
// The applet (wood) keeps its loading curtain up until the page ends startup.
// Unlike Miiverse, wood's endStartUp takes one argument (no-arg call throws
// "Arguments count is not match"); found by probing a real Wii U, 2026-10-09.
function wood(obj,fn,args){try{if(obj&&obj[fn])obj[fn].apply(obj,args)}catch(e){}}
wood(window.wiiuBrowser,'endStartUp',[true]);
wood(window.wiiuCurtain,'open',[]);
wood(window.wiiuDialog,'hideLoading',[]);
wood(window.wiiuBrowser,'showLoadingIcon',[false]);
function closeApp(){if(window.wiiuBrowser&&wiiuBrowser.closeApplication){wiiuBrowser.closeApplication()}else{history.back()}}
// GamePad only: the eShop applet keeps its own fixed screen on the TV ("Use the
// GamePad to input text"), even for fullscreen video in its native player
// (tested 2026-10-10: webkitEnterFullscreen works and wiiu.videoplayer.viewMode
// switches, but nothing reaches the TV). Videos play inline on the GamePad.
function play(u,n){var v=document.getElementById('vid');document.getElementById('vname').textContent=n;document.getElementById('player').style.display='block';v.src=u;v.play()}
function stop(){var v=document.getElementById('vid');v.pause();v.removeAttribute('src');v.load();document.getElementById('player').style.display='none'}
</script></body></html>`))

func handleWiiUShop(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		serveSamuraiMedia(w, r)
		return
	}
	log.Printf("wiiu shop: %s %s from %s (UA %q)", r.Method, r.URL.RequestURI(), realIP(r), r.UserAgent())
	if r.URL.Path != "/ninja/wood_index.html" && r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	cat, err := loadSamuraiCatalog()
	if err != nil {
		log.Printf("wiiu shop: catalog: %v", err)
		http.Error(w, "catalog unavailable", http.StatusServiceUnavailable)
		return
	}
	// Media links are relative to the host the page came from, so they keep
	// the page's own (http) scheme and stay inside the applet's allowlist.
	type pageVideo struct{ Name, Banner, URL string }
	type pageChannel struct {
		Name   string
		Videos []pageVideo
	}
	data := struct {
		Telops   []string
		Channels []pageChannel
	}{Telops: cat.Telops}
	for _, ch := range cat.Channels {
		pc := pageChannel{Name: ch.Name}
		for _, id := range ch.Videos {
			if v := cat.video(id); v != nil && v.MP4 != "" {
				pc.Videos = append(pc.Videos, pageVideo{Name: v.Name, Banner: "/media/" + v.Banner, URL: "/media/" + v.MP4})
			}
		}
		if len(pc.Videos) > 0 {
			data.Channels = append(data.Channels, pc)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := wiiuShopPage.Execute(w, data); err != nil {
		log.Printf("wiiu shop: render: %v", err)
	}
}

func serveSamuraiMedia(w http.ResponseWriter, r *http.Request) {
	rel := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/media/"))
	log.Printf("samurai media: %s %s (range %q) from %s", r.Method, rel, r.Header.Get("Range"), realIP(r))
	f, err := os.Open(filepath.Join(samuraiDir, "media", filepath.FromSlash(rel)))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	switch strings.ToLower(path.Ext(rel)) {
	case ".jpg", ".jpeg":
		w.Header().Set("Content-Type", "image/jpeg")
	case ".mp4":
		w.Header().Set("Content-Type", "video/mp4")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	http.ServeContent(w, r, "", st.ModTime(), f)
}
