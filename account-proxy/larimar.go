package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
)

// larimarPortalHost is the Wii U portal of Larimar, the secondary Miiverse
// (github.com/LarimarMiiverse, /var/www/larimar). Juxt stays the default:
// Juxt's discovery only hands this host to a Wii U whose player opted in from
// Juxt's settings (Redis key miiverse:instance:<pid>), and Larimar's own
// settings page deletes that key to switch back. Larimar has its own MariaDB
// database and logs players in through the account service, not Juxt.
//
// Wii U only for now: Larimar has no 3DS (n3ds) views yet, so there is no 3DS
// host and no 3DS-CA certificate. Its certificate is signed by the Wii U NN CA
// and must stay an explicit getCert case - see feedback_wildcard_cert_ordering.
const larimarPortalHost = "portal.larimar.nicochristmann.net"

var larimarProxy = func() *httputil.ReverseProxy {
	target, _ := url.Parse("http://127.0.0.1:8082")
	p := httputil.NewSingleHostReverseProxy(target)
	director := p.Director
	p.Director = func(r *http.Request) {
		host := r.Host
		director(r)
		// Larimar picks portal/n3ds/offdevice views from the subdomain.
		r.Host = host
		if ip := realIP(r); ip != "" {
			r.Header.Set("X-Real-IP", ip)
			r.Header.Set("X-Forwarded-For", ip)
		}
		r.Header.Set("X-Forwarded-Proto", "https")
	}
	p.ModifyResponse = func(resp *http.Response) error {
		// Same as handleOLV: Node's own Keep-Alive header never reaches a
		// real console from Nintendo's servers.
		resp.Header.Del("Keep-Alive")
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("Larimar proxy: %s %s upstream error: %v", r.Method, r.URL.Path, err)
		http.Error(w, "upstream error", http.StatusBadGateway)
	}
	return p
}()

// larimarMiiCDN serves the Mii faces account-proxy renders for every player
// (uploadMiiImages) from the same host as the page, like handleOLV does for
// Juxt: the Wii U only trusts our CA, not the S3 endpoint's.
func larimarMiiCDN(w http.ResponseWriter, r *http.Request) {
	resp, err := http.Get("https://olv-data.sos-de-fra-1.exo.io" + r.URL.Path)
	if err != nil {
		http.Error(w, "cdn error", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for _, k := range []string{"Content-Type", "Content-Length", "Cache-Control", "ETag", "Last-Modified"} {
		if v := resp.Header.Get(k); v != "" {
			w.Header().Set(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleLarimar(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/mii/") {
		larimarMiiCDN(w, r)
		return
	}
	if r.Method == http.MethodPost {
		// A switch back to Juxt changes what discovery returns, which relays
		// may hold in their content cache.
		defer announceInvalidation("content:all")
	}
	larimarProxy.ServeHTTP(w, r)
	log.Printf("Larimar proxy: %s %s", r.Method, r.URL.Path)
}
