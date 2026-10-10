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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	minio "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
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
#player{display:none;position:fixed;top:0;left:0;right:0;bottom:0;background:#000;text-align:center;overflow-y:auto}
#player video{width:1280px;height:600px;max-width:100%;background:#000}
#player h2{margin:8px 0 0;font-size:24px}
#player .err{display:none;font-size:22px;color:#ff8a80}
#player #back{position:absolute;top:12px;right:12px;font-size:22px;padding:10px 22px;border:0;border-radius:8px}
.v .st{margin:-6px 12px 12px;font-size:15px;color:#aaa}
#social{display:none;text-align:left;max-width:900px;margin:12px auto 40px;padding:0 20px}
#social .bar{display:-webkit-box;-webkit-box-align:center;margin-bottom:14px}
#stats{-webkit-box-flex:1;font-size:20px;color:#ccc}
#social button{font-size:22px;padding:10px 22px;border:0;border-radius:8px;background:#333;color:#fff;margin-left:10px}
#social button.on{background:#e2231a}
.cform{display:-webkit-box;margin-bottom:10px}
#ctext{-webkit-box-flex:1;font-size:22px;padding:10px;border:0;border-radius:8px}
#smsg{color:#ff8a80;font-size:18px;min-height:22px;margin:4px 0 10px}
.c{border-top:1px solid #333;padding:10px 0;font-size:20px;word-wrap:break-word}
.c b{color:#fff}.c span{color:#888;font-size:16px;margin-left:8px}.c div{color:#ddd;margin-top:4px;white-space:pre-wrap}
</style></head><body>
<div id="top"><h1>Revivetendo TV</h1><button id="close" onclick="closeApp()">Close</button></div>
{{range .Telops}}<div id="telop">{{.}}</div>{{end}}
{{range .Channels}}<div class="ch"><h2>{{.Name}}</h2><div class="row">
{{range .Videos}}<a class="v" href="#" onclick="play('{{.URL}}','{{.Name}}',{{.ID}},{{.Social}});return false"><img src="{{.Banner}}" alt=""><p>{{.Name}}</p>{{if .Social}}<p class="st">{{.Views}} views &middot; {{.Likes}} likes</p>{{end}}</a>{{end}}
</div></div>{{end}}
<div id="player"><video id="vid" controls onerror="vidErr()"></video><h2 id="vname"></h2><p id="verr" class="err">This video can't be loaded right now. Please try again later.</p><button id="back" onclick="stop()">Back</button>
<div id="social"><div class="bar"><span id="stats"></span><button id="like" onclick="like()">Like</button></div>
<div class="cform"><input id="ctext" type="text" maxlength="200" placeholder="Write a comment..."><button onclick="comment()">Post</button></div>
<p id="smsg"></p><div id="clist"></div></div></div>
<script>
// The applet (wood) keeps its loading screen up until the page ends startup.
// Unlike Miiverse, wood's endStartUp takes one argument (no-arg call throws
// "Arguments count is not match"); found by probing a real Wii U, 2026-10-09.
// wiiuCurtain is the TV cover ("Use the GamePad to input text"), not the
// loading screen: open() puts it up, close() takes it down, after which the TV
// mirrors the page and plays videos too (found on a real Wii U, 2026-10-10).
function wood(obj,fn,args){try{if(obj&&obj[fn])obj[fn].apply(obj,args)}catch(e){}}
wood(window.wiiuBrowser,'endStartUp',[true]);
wood(window.wiiuCurtain,'close',[]);
wood(window.wiiuDialog,'hideLoading',[]);
wood(window.wiiuBrowser,'showLoadingIcon',[false]);
function closeApp(){if(window.wiiuBrowser&&wiiuBrowser.closeApplication){wiiuBrowser.closeApplication()}else{history.back()}}
function vidErr(){document.getElementById('verr').style.display='block'}
function $(i){return document.getElementById(i)}
function play(u,n,id,social){$('verr').style.display='none';var v=$('vid');$('vname').textContent=n;$('player').style.display='block';$('player').scrollTop=0;v.src=u;v.play();
curID=id;$('social').style.display=social?'block':'none';$('clist').innerHTML='';$('stats').textContent='';$('smsg').textContent='';if(social)tvLoad()}
// Views, likes and comments (samurai_social.go). The server checks the
// account against this console's recent logins.
var curID=0,PID='';
try{if(window.wiiuNNA){var p=wiiuNNA.principalId;PID=String(typeof p==='function'?wiiuNNA.principalId():p)}}catch(e){}
function tvReq(method,path,body,cb){var x=new XMLHttpRequest();x.open(method,'/ninja/tv/'+path+'?id='+curID,true);x.setRequestHeader('X-TV-PID',PID);x.setRequestHeader('Content-Type','application/json');
x.onreadystatechange=function(){if(x.readyState!==4)return;var d={};try{d=JSON.parse(x.responseText)}catch(e){}if(x.status===200)cb(d);else $('smsg').textContent=d.error||'Something went wrong, please try again.'};x.send(body?JSON.stringify(body):null)}
function tvLoad(){tvReq('GET','video',null,tvRender)}
function tvRender(d){$('smsg').textContent='';$('stats').textContent=d.views+' views \u00b7 '+d.likes+' likes \u00b7 '+d.commentCount+' comments';
var b=$('like');b.textContent=d.liked?'Liked':'Like';b.className=d.liked?'on':'';
var l=$('clist');l.innerHTML='';for(var i=0;i<d.comments.length;i++){var c=d.comments[i],e=document.createElement('div'),h=document.createElement('b'),t=document.createElement('span'),m=document.createElement('div');
e.className='c';h.textContent=c.name||'?';t.textContent=c.date;m.textContent=c.body;e.appendChild(h);e.appendChild(t);e.appendChild(m);l.appendChild(e)}
if(!d.comments.length){var n=document.createElement('div');n.className='c';n.textContent='No comments yet.';l.appendChild(n)}}
function like(){tvReq('POST','like',{},tvRender)}
function comment(){var t=$('ctext').value.replace(/^\s+|\s+$/g,'');if(!t)return;tvReq('POST','comment',{body:t},function(d){$('ctext').value='';tvRender(d)})}
function stop(){var v=document.getElementById('vid');v.pause();v.removeAttribute('src');v.load();document.getElementById('player').style.display='none'}
</script></body></html>`))

func handleWiiUShop(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/media/") {
		serveSamuraiMedia(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/ninja/tv/") {
		handleTVAPI(w, r)
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
	type pageVideo struct {
		Name, Banner, URL string
		ID                int64
		Social            bool // an upload: has views, likes and comments
		Views, Likes      int64
	}
	type pageChannel struct {
		Name   string
		Videos []pageVideo
	}
	data := struct {
		Telops   []string
		Channels []pageChannel
	}{Telops: cat.Telops}
	var uploads []int64
	for _, v := range cat.Videos {
		if id, ok := tvUploadID(v.ID); ok {
			uploads = append(uploads, id)
		}
	}
	stats := tvStatsFor(uploads)
	for _, ch := range cat.Channels {
		pc := pageChannel{Name: ch.Name}
		for _, id := range ch.Videos {
			if v := cat.video(id); v != nil && v.MP4 != "" {
				pv := pageVideo{Name: v.Name, Banner: "/media/" + v.Banner, URL: "/media/" + v.MP4, ID: v.ID}
				if uid, ok := tvUploadID(v.ID); ok {
					st := stats[uid]
					pv.Social, pv.Views, pv.Likes = true, st.Views, st.Likes
				}
				pc.Videos = append(pc.Videos, pv)
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

// Uploaded videos (relay-admin/videos*.go) are listed in the catalog as
// u/{id}/{file} and stored in S3 as videos/{id}/{file} in a private bucket;
// the consoles can't reach Exoscale themselves, so they are streamed through
// here, and only while the catalog lists them (videos in review stay private).
const samuraiVideoBucket = "revivetendo-tv"

var (
	samuraiS3Once   sync.Once
	samuraiS3Client *minio.Client
	samuraiUploadRe = regexp.MustCompile(`^/u/([0-9]+)/(video\.moflex|video\.mp4|thumb\.jpg|banner\.jpg)$`)
)

func samuraiS3() *minio.Client {
	samuraiS3Once.Do(func() {
		if s3Endpoint == "" || s3AccessKey == "" {
			return
		}
		c, err := minio.New(s3Endpoint, &minio.Options{Creds: credentials.NewStaticV4(s3AccessKey, s3SecretKey, ""), Region: s3Region, Secure: true})
		if err != nil {
			log.Printf("samurai: S3 client: %v", err)
			return
		}
		samuraiS3Client = c
	})
	return samuraiS3Client
}

// samuraiListed reports whether the catalog points at media path p (e.g. "u/7/video.mp4").
func samuraiListed(p string) bool {
	cat, err := loadSamuraiCatalog()
	if err != nil {
		return false
	}
	for _, v := range cat.Videos {
		if v.File == p || v.MP4 == p || v.Thumbnail == p || v.Banner == p {
			return true
		}
	}
	return false
}

func samuraiContentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".mp4":
		return "video/mp4"
	}
	return "application/octet-stream"
}

type countingWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	return n, err
}

func serveSamuraiUpload(w http.ResponseWriter, r *http.Request, rel string, m []string) {
	if !samuraiListed(strings.TrimPrefix(rel, "/")) {
		http.NotFound(w, r)
		return
	}
	if m[2] == "video.moflex" || m[2] == "video.mp4" {
		tvCountView(r, m[1])
		// How fast the console really receives the video: a stream it can't
		// download faster than its bitrate stutters whatever the encoding.
		cw := &countingWriter{ResponseWriter: w}
		w = cw
		start := time.Now()
		defer func() {
			d := time.Since(start).Seconds()
			if d > 0 && cw.n > 0 {
				log.Printf("samurai media: sent %s %.1f MB in %.1fs = %.0f kbps to %s", rel, float64(cw.n)/1048576, d, float64(cw.n)*8/1000/d, realIP(r))
			}
		}()
	}
	w.Header().Set("Content-Type", samuraiContentType(m[2]))
	// A conversion that never reached S3 (no S3 configured) is still local.
	if f, err := os.Open(filepath.Join(samuraiDir, "submissions", m[1], m[2])); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil {
			http.ServeContent(w, r, "", st.ModTime(), f)
			return
		}
	}
	c := samuraiS3()
	if c == nil {
		http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
		return
	}
	obj, err := c.GetObject(r.Context(), samuraiVideoBucket, "videos/"+m[1]+"/"+m[2], minio.GetObjectOptions{})
	if err == nil {
		defer obj.Close()
		var st minio.ObjectInfo
		if st, err = obj.Stat(); err == nil {
			http.ServeContent(w, r, "", st.LastModified, obj) // minio.Object seeks with ranged GETs
			return
		}
	}
	log.Printf("samurai media: S3 read %s failed: %v", rel, err)
	http.Error(w, "storage unavailable", http.StatusServiceUnavailable)
}

func serveSamuraiMedia(w http.ResponseWriter, r *http.Request) {
	rel := path.Clean("/" + strings.TrimPrefix(r.URL.Path, "/media/"))
	log.Printf("samurai media: %s %s (range %q) from %s", r.Method, rel, r.Header.Get("Range"), realIP(r))
	if m := samuraiUploadRe.FindStringSubmatch(rel); m != nil {
		serveSamuraiUpload(w, r, rel, m)
		return
	}
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
	w.Header().Set("Content-Type", samuraiContentType(rel))
	http.ServeContent(w, r, "", st.ModTime(), f)
}
