package main

// Translations for the public dashboard pages: i18n/i18n.js picks the language in
// the browser and loads i18n/lang/<code>.js, which hold hand-written strings keyed
// by the pages' data-i18n attributes (English stays in the templates). The admin
// pages are not translated.

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
)

//go:embed i18n
var i18nFiles embed.FS

// i18nScript is the tag every translated page puts in <head>.
const i18nScript = `<script src="/inkay/i18n/i18n.js"></script>`

func init() {
	sub, err := fs.Sub(i18nFiles, "i18n")
	if err != nil {
		panic(err)
	}
	// i18n.js asks for lang/<code>.js?v=<hash of all files>, so the language files can
	// stay cached for an hour without hiding a deploy; i18n.js itself is revalidated.
	h := sha256.New()
	fs.WalkDir(sub, ".", func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := fs.ReadFile(sub, path)
			h.Write([]byte(path))
			h.Write(b)
		}
		return nil
	})
	version := hex.EncodeToString(h.Sum(nil))[:12]
	loader, err := fs.ReadFile(sub, "i18n.js")
	if err != nil {
		panic(err)
	}
	loader = bytes.Replace(loader, []byte("__I18N_VERSION__"), []byte(version), 1)
	files := http.StripPrefix("/i18n/", http.FileServer(http.FS(sub)))
	http.HandleFunc("/i18n/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/i18n/i18n.js" {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("ETag", `"`+version+`"`)
			if r.Header.Get("If-None-Match") == `"`+version+`"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Write(loader)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=3600")
		files.ServeHTTP(w, r)
	})
}

// msgKeys maps the fixed ?msg= notices of the /my/ pages to their i18n keys
// (a notice that is not listed stays English).
var msgKeys = map[string]string{
	"Discord Rich Presence was cancelled": "msg.rp_cancelled",
	"Discord refused the presence permission (invalid_scope) - the Revivetendo app is not set up for it yet": "msg.rp_scope",
	"That request expired or does not belong to this account - please try again":                             "msg.rp_expired",
	"Link your PNID to Discord with /link_pnid first":                                                        "msg.rp_link_first",
	"Could not reach Discord":             "msg.rp_unreachable",
	"Discord did not accept the login":    "msg.rp_login",
	"Could not read your Discord account": "msg.rp_account",
	"That Discord account is not the one linked to your PNID - sign in to Discord with the linked account": "msg.rp_wrong_account",
	"Could not save the link": "msg.save_failed",
	"Discord Rich Presence enabled - it shows while your console is online":           "msg.rp_on",
	"Discord Rich Presence turned off":                                                "msg.rp_off",
	"Patreon linking was cancelled":                                                   "msg.pt_cancelled",
	"That link request expired or does not belong to this account - please try again": "msg.pt_expired",
	"Could not reach Patreon":                                                         "msg.pt_unreachable",
	"Patreon did not accept the login":                                                "msg.pt_login",
	"Could not read your Patreon account":                                             "msg.pt_account",
	"That Patreon account is already linked to another PNID":                          "msg.pt_taken",
	"Patreon linked":   "msg.pt_linked",
	"Patreon unlinked": "msg.pt_unlinked",
}
