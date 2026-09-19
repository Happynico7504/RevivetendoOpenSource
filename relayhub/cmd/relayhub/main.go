// relayhub is the main instance's side of the relay network: it serves the
// encrypted relay API, manages the relay registry, and generates the DNS
// server's region config from it.
package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rsa"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relayhub"
	"github.com/Happynico7504/relaylink"
)

func usage() {
	fmt.Fprint(os.Stderr, `usage: relayhub <command> [flags]

  keygen      [-out FILE] [-bits N]             create the main's RSA key pair
  serve       [-listen ADDR] [-internal ADDR] [-key FILE]
  relay add   -id ID -region R -host H -url MAIN_URL -bundle FILE [-health-port P] [-name N]
  relay list
  relay enable|disable|delete ID
  dns-config  [-base FILE] [-o FILE]            regiondns config from the registry
  invalidate  TAG...                            drop cached entries on all relays
  call        -bundle FILE [-method M] PATH     make one request as a relay (for testing)
  stream-ping -bundle FILE [-n N] [-addr H:P]   measure the real-time stream latency to the main
  certs       [-dir DIR] [-default NAME]        show which certificates relays may fetch
  pubkey      [-key FILE]                       (re)write main-public.pem next to the main key
  release keygen [-out FILE]                    create the OTA release signing key (keep it OFF the main if you can)
  release sign   -binary FILE -version N -os linux -arch amd64 [-label L] [-key FILE] [-out DIR] [-force]
  release list   [-dir DIR]                     show what the hub serves

Database: PN_WUC_POSTGRES_URI (loaded from -env, default ../wiiu-chat-secure/.env).
`)
	os.Exit(2)
}

func defaultKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".relayhub", "main-key.pem")
}

func openDB(envFile string) *sql.DB {
	godotenv.Load(envFile)
	uri := os.Getenv("PN_WUC_POSTGRES_URI")
	if uri == "" {
		log.Fatal("PN_WUC_POSTGRES_URI not set")
	}
	db, err := sql.Open("postgres", uri)
	if err != nil {
		log.Fatal(err)
	}
	return db
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "keygen":
		cmdKeygen(args)
	case "serve":
		cmdServe(args)
	case "relay":
		cmdRelay(args)
	case "dns-config":
		cmdDNSConfig(args)
	case "invalidate":
		cmdInvalidate(args)
	case "call":
		cmdCall(args)
	case "stream-ping":
		cmdStreamPing(args)
	case "certs":
		cmdCerts(args)
	case "pubkey":
		cmdPubkey(args)
	case "release":
		cmdRelease(args)
	default:
		usage()
	}
}

func cmdKeygen(args []string) {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	out := fs.String("out", defaultKeyPath(), "private key file")
	bits := fs.Int("bits", 3072, "RSA key size")
	fs.Parse(args)
	if _, err := os.Stat(*out); err == nil {
		log.Fatalf("%s already exists; refusing to overwrite the main key (delete it deliberately if you mean to rotate)", *out)
	}
	k, err := relaylink.GenerateMainKey(*bits)
	if err != nil {
		log.Fatal(err)
	}
	pem, _ := relaylink.MarshalPrivatePEM(k)
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(*out, pem, 0o600); err != nil {
		log.Fatal(err)
	}
	pub, _ := relaylink.MarshalPublicPEM(&k.PublicKey)
	pubPath := publicKeyPath(*out)
	if err := os.WriteFile(pubPath, pub, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (0600) and %s\npublic key:\n%s", *out, pubPath, pub)
}

// publicKeyPath is where the public half lives: <dir>/main-public.pem. The
// relay-admin dashboard reads it to build relay bundles without ever touching
// the private key.
func publicKeyPath(privPath string) string {
	return filepath.Join(filepath.Dir(privPath), "main-public.pem")
}

func cmdPubkey(args []string) {
	fs := flag.NewFlagSet("pubkey", flag.ExitOnError)
	keyPath := fs.String("key", defaultKeyPath(), "main RSA private key")
	fs.Parse(args)
	k := loadKey(*keyPath)
	pub, _ := relaylink.MarshalPublicPEM(&k.PublicKey)
	out := publicKeyPath(*keyPath)
	if err := os.WriteFile(out, pub, 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s\n%s", out, pub)
}

func loadKey(path string) *rsa.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("main key: %v (create it with: relayhub keygen)", err)
	}
	k, err := relaylink.ParsePrivatePEM(raw)
	if err != nil {
		log.Fatal(err)
	}
	return k
}

func cmdServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := fs.String("listen", "0.0.0.0:7777", "relay API listen address")
	internal := fs.String("internal", "127.0.0.1:9401", "local-only invalidation endpoint")
	wscEdge := fs.String("wsc-edge", "http://127.0.0.1:9451", "wsc-secure's edge endpoint for relay-terminated WSC sessions (empty = off)")
	keyPath := fs.String("key", defaultKeyPath(), "main RSA private key")
	envFile := fs.String("env", "../wiiu-chat-secure/.env", "env file with PN_WUC_POSTGRES_URI")
	redisAddr := fs.String("redis", "127.0.0.1:6379", "Redis for replay protection")
	certDir := fs.String("certs", "/nico-pretendo-bridge/certs", "directory of certificates relays may fetch (\"\" disables)")
	defCert := fs.String("default-cert", "olv-nicochristmann-net", "certificate relays serve when the SNI matches nothing")
	releasesDir := fs.String("releases", defaultReleasesDir(), "directory of signed relayd releases (\"\" disables OTA)")
	streamListen := fs.String("stream-listen", "0.0.0.0:7778", "real-time relay stream listen address (\"\" disables)")
	geoPath := fs.String("geoip", "/usr/local/share/revivetendo-dns/dbip-country-lite.mmdb", "GeoIP database used to choose a relay for a console (\"\" disables regional NEX routing)")
	nexRoot := fs.String("nex-root", "/nico-pretendo-bridge", "repository root holding the *-authentication/.env files")
	fs.Parse(args)

	db := openDB(*envFile)
	reg := &relayhub.PGRegistry{DB: db}
	if err := reg.Init(context.Background()); err != nil {
		log.Fatalf("registry schema: %v", err)
	}
	invlog := relayhub.NewInvalidationLog(10000)
	hub := &relayhub.Hub{
		Src: &relayhub.PGSource{DB: db}, Log: invlog,
		Fwd: &relayhub.Forwarder{Backends: relayhub.DefaultBackends()},
	}
	if *releasesDir != "" {
		hub.Rel = &relayhub.ReleaseStore{Dir: *releasesDir}
	}
	if *certDir != "" {
		hub.Certs = &relayhub.CertStore{Dir: *certDir, Default: *defCert, Routes: relayhub.DefaultCertRoutes()}
	}
	hub.Fwd.OnWrite = func() { invlog.Append([]string{relaylink.ContentTag}) }
	keys := &relayhub.KeyLookup{Reg: reg}
	srv := &relaylink.Server{
		Priv:     loadKey(*keyPath),
		RelayKey: keys.Lookup,
		Replay:   &relayhub.RedisReplay{Client: redis.NewClient(&redis.Options{Addr: *redisAddr})},
	}

	// Real-time streams: one persistent encrypted connection per relay. Data
	// changes are pushed over it the moment they are announced.
	streams := relayhub.NewStreamHub()
	streams.OnRelayUp = func(id string) { log.Printf("stream: relay %q connected", id) }
	streams.OnRelayDown = func(id string, pids []uint32) {
		log.Printf("stream: relay %q disconnected (%d players were connected through it)", id, len(pids))
	}
	invlog.OnAppend = streams.PushInvalidation
	// Regional NEX authentication: choose a relay for a console, hand it the
	// credential, and answer its pulls only for consoles sent to it.
	var assigner *relayhub.NexAssigner
	if *geoPath != "" {
		geo, gerr := relayhub.OpenMMDBGeo(*geoPath)
		if gerr != nil {
			log.Printf("relayhub: regional NEX routing OFF (geoip: %v)", gerr)
		} else {
			var (
				gmu    sync.Mutex
				gcache map[string]relaylink.NexGame
				gtime  time.Time
			)
			// ~/.relayhub/nex-force-pids: consoles (or "*" = everyone) that are sent to a relay
			// whatever the region rules say. Re-read every few seconds, so it can be changed
			// (or deleted to go back to the region rules) without restarting the hub.
			var (
				fmu    sync.Mutex
				fcache map[uint32]bool
				ftime  time.Time
			)
			forcePath := filepath.Join(filepath.Dir(*keyPath), "nex-force-pids")
			// ~/.relayhub/wsc-edge-pids: WSC consoles whose session is terminated on the relay
			// (the WSC edge). Same format and reload behaviour; empty or missing = nobody.
			var (
				emu    sync.Mutex
				ecache map[uint32]bool
				etime  time.Time
			)
			edgePath := filepath.Join(filepath.Dir(*keyPath), "wsc-edge-pids")
			assigner = &relayhub.NexAssigner{
				Streams: streams, Registry: reg, Geo: geo, Logf: log.Printf,
				EdgePIDs: func() map[uint32]bool {
					emu.Lock()
					defer emu.Unlock()
					if ecache == nil || time.Since(etime) > 5*time.Second {
						raw, _ := os.ReadFile(edgePath)
						ecache, etime = relayhub.ParseForcePIDs(string(raw)), time.Now()
					}
					return ecache
				},
				ForcePIDs: func() map[uint32]bool {
					fmu.Lock()
					defer fmu.Unlock()
					if fcache == nil || time.Since(ftime) > 5*time.Second {
						raw, _ := os.ReadFile(forcePath)
						fcache, ftime = relayhub.ParseForcePIDs(string(raw)), time.Now()
					}
					return fcache
				},
				Games: func() map[string]relaylink.NexGame {
					gmu.Lock()
					defer gmu.Unlock()
					if gcache == nil || time.Since(gtime) > 30*time.Second {
						gcache, gtime = relayhub.LoadNexGames(*nexRoot, os.Getenv), time.Now()
					}
					return gcache
				},
			}
			assigner.Register()
			names := []string{}
			for n := range assigner.Games() {
				names = append(names, n)
			}
			sort.Strings(names)
			log.Printf("relayhub: regional NEX routing ON for %v", names)
		}
	}
	// WSC edge: relays that terminate players' PRUDP sessions reach wsc-secure through here.
	// Inert unless wsc-secure runs with the edge enabled and a relay runs the edge component.
	var edge *relayhub.EdgeBridge
	if *wscEdge != "" {
		edge = &relayhub.EdgeBridge{Streams: streams, Main: *wscEdge, Logf: log.Printf}
		edge.Register()
	}
	if *streamListen != "" {
		sln, err := net.Listen("tcp", *streamListen)
		if err != nil {
			log.Fatalf("stream listen: %v", err)
		}
		go func() { log.Fatal(srv.ServeStream(sln, relaylink.StreamOptions{}, streams.Handlers, streams.OnConn)) }()
		log.Printf("relayhub: real-time streams on %s", *streamListen)
	}

	// Local-only: the other services tell the hub that data changed.
	go func() {
		mux := http.NewServeMux()
		mux.HandleFunc("/assign", func(w http.ResponseWriter, r *http.Request) {
			// Called by account-proxy while it answers a console's nex_token request.
			// 204 (or any error) means: authenticate at the main as usual.
			if r.Method != http.MethodPost || assigner == nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var req relayhub.AssignRequest
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&req); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), 900*time.Millisecond)
			defer cancel()
			res, err := assigner.Assign(ctx, req)
			if err != nil {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			log.Printf("nex: %s pid=%d -> relay %s", res.Game, req.PID, res.Relay)
			json.NewEncoder(w).Encode(res)
		})
		if edge != nil {
			mux.HandleFunc("/edge/out", edge.Out)
		}
		mux.HandleFunc("/invalidate", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			var body struct {
				Tags []string `json:"tags"`
			}
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || len(body.Tags) == 0 {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			fmt.Fprintf(w, `{"seq":%d}`+"\n", invlog.Append(body.Tags))
		})
		log.Fatal((&http.Server{Addr: *internal, Handler: mux, ReadHeaderTimeout: 5 * time.Second}).ListenAndServe())
	}()

	log.Printf("relayhub: relay API on %s, invalidations on %s", *listen, *internal)
	log.Fatal((&http.Server{
		Addr: *listen, Handler: srv.RPCHandler(hub.Dispatch),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second,
	}).ListenAndServe())
}

func cmdRelay(args []string) {
	if len(args) == 0 {
		usage()
	}
	sub, rest := args[0], args[1:]
	envFile := "../wiiu-chat-secure/.env"
	ctx := context.Background()

	if sub == "add" {
		fs := flag.NewFlagSet("relay add", flag.ExitOnError)
		id := fs.String("id", "", "relay id, e.g. us-1")
		region := fs.String("region", "", fmt.Sprintf("region %v", relayhub.RegionNames()))
		host := fs.String("host", "", "hostname or IP the DNS server hands out")
		name := fs.String("name", "", "display name")
		port := fs.Int("health-port", 0, "TCP port the DNS server probes (0 = none)")
		mainURL := fs.String("url", "", "URL the relay uses to reach this main, e.g. http://main.example:7777")
		bundle := fs.String("bundle", "", "file to write the relay's secret bundle to")
		keyPath := fs.String("key", defaultKeyPath(), "main RSA private key")
		fs.String("env", envFile, "env file")
		fs.Parse(rest)
		if *mainURL == "" || *bundle == "" {
			log.Fatal("-url and -bundle are required")
		}
		if _, err := os.Stat(*bundle); err == nil {
			log.Fatalf("%s exists; refusing to overwrite", *bundle)
		}
		pub, priv, err := relaylink.NewRelayIdentity()
		if err != nil {
			log.Fatal(err)
		}
		r := &relayhub.Relay{ID: *id, Name: *name, Region: *region, Host: *host, HealthPort: *port, PublicKey: pub, Enabled: true}
		if err := r.Validate(); err != nil {
			log.Fatal(err)
		}
		mainKey := loadKey(*keyPath)
		b, err := relaylink.NewBundle(*id, priv, &mainKey.PublicKey, *mainURL)
		if err != nil {
			log.Fatal(err)
		}
		if rk, err := os.ReadFile(defaultReleaseKeyPath() + ".pub"); err == nil {
			b.ReleasePublicKey = strings.TrimSpace(string(rk)) // enables OTA updates on this relay
		}
		reg := &relayhub.PGRegistry{DB: openDB(envFile)}
		if err := reg.Init(ctx); err != nil {
			log.Fatal(err)
		}
		if err := reg.Add(ctx, r); err != nil {
			log.Fatalf("add: %v", err)
		}
		raw, _ := json.MarshalIndent(b, "", "  ")
		if err := os.WriteFile(*bundle, raw, 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("relay %q added (region %s). Secret bundle written to %s (0600):\ncopy it to the relay over a secure channel and delete this copy.\n", *id, *region, *bundle)
		return
	}

	reg := &relayhub.PGRegistry{DB: openDB(envFile)}
	if err := reg.Init(ctx); err != nil {
		log.Fatal(err)
	}
	switch sub {
	case "list":
		rs, err := reg.List(ctx)
		if err != nil {
			log.Fatal(err)
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tREGION\tHOST\tHEALTH\tENABLED\tLAST SEEN")
		for _, r := range rs {
			seen := "never"
			if r.LastSeen != nil {
				seen = r.LastSeen.Format(time.RFC3339)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%v\t%s\n", r.ID, r.Region, r.Host, r.HealthPort, r.Enabled, seen)
		}
		tw.Flush()
	case "enable", "disable", "delete":
		if len(rest) != 1 {
			usage()
		}
		var err error
		switch sub {
		case "enable":
			err = reg.SetEnabled(ctx, rest[0], true)
		case "disable":
			err = reg.SetEnabled(ctx, rest[0], false)
		case "delete":
			err = reg.Delete(ctx, rest[0])
		}
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println("ok (a running hub notices within ~10s)")
	default:
		usage()
	}
}

func cmdDNSConfig(args []string) {
	fs := flag.NewFlagSet("dns-config", flag.ExitOnError)
	base := fs.String("base", "/etc/revivetendo-dns/regiondns.json", "existing regiondns config to keep (default region, listen, zone...)")
	out := fs.String("o", "", "write here (atomically) instead of stdout")
	envFile := fs.String("env", "../wiiu-chat-secure/.env", "env file")
	fs.Parse(args)

	raw, err := os.ReadFile(*base)
	if err != nil {
		log.Fatal(err)
	}
	reg := &relayhub.PGRegistry{DB: openDB(*envFile)}
	if err := reg.Init(context.Background()); err != nil {
		log.Fatal(err)
	}
	rs, err := reg.List(context.Background())
	if err != nil {
		log.Fatal(err)
	}
	conf, err := relayhub.BuildDNSConfig(raw, rs)
	if err != nil {
		log.Fatal(err)
	}
	conf = append(conf, '\n')
	if *out == "" {
		os.Stdout.Write(conf)
		return
	}
	tmp := *out + ".tmp"
	if err := os.WriteFile(tmp, conf, 0o644); err != nil {
		log.Fatal(err)
	}
	if err := os.Rename(tmp, *out); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (regiondns reloads it automatically)\n", *out)
}

func cmdInvalidate(args []string) {
	if len(args) == 0 {
		usage()
	}
	body, _ := json.Marshal(map[string]any{"tags": args})
	resp, err := http.Post("http://127.0.0.1:9401/invalidate", "application/json", bytes.NewReader(body))
	if err != nil {
		log.Fatalf("hub not reachable: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	buf.ReadFrom(resp.Body)
	fmt.Print(buf.String())
}

func cmdCall(args []string) {
	fs := flag.NewFlagSet("call", flag.ExitOnError)
	bundle := fs.String("bundle", "", "relay bundle file")
	method := fs.String("method", "GET", "HTTP method")
	fs.Parse(args)
	if *bundle == "" || fs.NArg() != 1 {
		usage()
	}
	raw, err := os.ReadFile(*bundle)
	if err != nil {
		log.Fatal(err)
	}
	b, err := relaylink.ParseBundle(raw)
	if err != nil {
		log.Fatal(err)
	}
	c, err := b.Client()
	if err != nil {
		log.Fatal(err)
	}
	resp, err := c.Call(context.Background(), *method, fs.Arg(0), nil)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("status=%d ttl=%ds tags=%v\n%s\n", resp.Status, resp.TTL, resp.Tags, resp.Body)
}

func cmdCerts(args []string) {
	fs := flag.NewFlagSet("certs", flag.ExitOnError)
	dir := fs.String("dir", "/nico-pretendo-bridge/certs", "certificate directory")
	def := fs.String("default", "olv-nicochristmann-net", "default certificate name")
	fs.Parse(args)
	m, err := (&relayhub.CertStore{Dir: *dir, Default: *def, Routes: relayhub.DefaultCertRoutes()}).Manifest()
	if err != nil {
		log.Fatal(err)
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tEXPIRES\tNAMES")
	for _, c := range m.Certs {
		fmt.Fprintf(tw, "%s\t%s\t%v\n", c.Name, time.Unix(c.NotAfter, 0).Format("2006-01-02"), c.DNSNames)
	}
	tw.Flush()
	fmt.Println("\nSNI routes (first match wins):")
	for _, r := range m.Routes {
		fmt.Printf("  %-6s %-32s -> %s\n", r.Match, r.Value, r.Cert)
	}
	fmt.Printf("default: %q\n(CA certificates and every non-matching/backup/CSR file are excluded by design)\n", m.Default)
}

func defaultReleasesDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".relayhub", "releases")
}

func defaultReleaseKeyPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".relayhub", "release-key")
}

func loadReleaseKey(path string) ed25519.PrivateKey {
	raw, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("release key: %v (create it with: relayhub release keygen)", err)
	}
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(b) != ed25519.PrivateKeySize {
		log.Fatal("release key file is not a base64 Ed25519 private key")
	}
	return ed25519.PrivateKey(b)
}

func cmdRelease(args []string) {
	if len(args) == 0 {
		usage()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "keygen":
		fs := flag.NewFlagSet("release keygen", flag.ExitOnError)
		out := fs.String("out", defaultReleaseKeyPath(), "private key file (public half is written next to it as <file>.pub)")
		fs.Parse(rest)
		if _, err := os.Stat(*out); err == nil {
			log.Fatalf("%s exists; refusing to overwrite the release key (relays pin its public half)", *out)
		}
		pub, priv, err := relaylink.NewRelayIdentity()
		if err != nil {
			log.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
			log.Fatal(err)
		}
		if err := os.WriteFile(*out, []byte(base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
			log.Fatal(err)
		}
		pubB64 := relaylink.EncodeReleasePublicKey(pub)
		if err := os.WriteFile(*out+".pub", []byte(pubB64+"\n"), 0o644); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("wrote %s (0600) and %s.pub\nrelease public key (new relay bundles pin this): %s\nBest practice: move the private key off this machine and sign releases elsewhere.\n", *out, *out, pubB64)
	case "sign":
		fs := flag.NewFlagSet("release sign", flag.ExitOnError)
		component := fs.String("component", relaylink.ComponentRelayd, "what is being published: relayd, or a separately shipped binary such as wscedge")
		binPath := fs.String("binary", "", "binary to publish")
		version := fs.Uint64("version", 0, "release version (strictly increasing)")
		goos := fs.String("os", "linux", "target OS")
		goarch := fs.String("arch", "amd64", "target architecture")
		label := fs.String("label", "", "free-text label")
		keyPath := fs.String("key", defaultReleaseKeyPath(), "release signing key")
		out := fs.String("out", defaultReleasesDir(), "releases directory (use any directory to sign elsewhere and copy it over)")
		force := fs.Bool("force", false, "allow a version that is not newer than the published one")
		fs.Parse(rest)
		bin, err := os.ReadFile(*binPath)
		if err != nil {
			log.Fatal(err)
		}
		m, err := relayhub.PublishComponent(*out, *component, bin, *version, *label, *goos, *goarch, loadReleaseKey(*keyPath), *force)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("published %s %s/%s version %d (%d bytes, sha256 %s) to %s\n", m.ComponentName(), m.OS, m.Arch, m.Version, m.Size, m.SHA256[:16], *out)
	case "list":
		fs := flag.NewFlagSet("release list", flag.ExitOnError)
		dir := fs.String("dir", defaultReleasesDir(), "releases directory")
		fs.Parse(rest)
		store := &relayhub.ReleaseStore{Dir: *dir}
		found := false
		for _, comp := range store.Components() {
			for _, plat := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}, {"linux", "arm"}} {
				if m, err := store.ManifestFor(comp, plat[0], plat[1]); err == nil {
					fmt.Printf("%-8s %s/%s  version %d  %d bytes  sha256 %s  %s\n", comp, m.OS, m.Arch, m.Version, m.Size, m.SHA256[:16], m.Label)
					found = true
				}
			}
		}
		if !found {
			fmt.Println("no releases published")
		}
	default:
		usage()
	}
}

func cmdStreamPing(args []string) {
	fs := flag.NewFlagSet("stream-ping", flag.ExitOnError)
	bundle := fs.String("bundle", "", "relay bundle file")
	n := fs.Int("n", 20, "number of echo calls")
	addr := fs.String("addr", "", "stream address (default: the bundle's host, port 7778)")
	rawAddr := fs.String("raw", "", "also measure a plain TCP echo server at this address, interleaved sample by sample (fair A/B comparison)")
	fs.Parse(args)
	raw, err := os.ReadFile(*bundle)
	if err != nil {
		log.Fatal(err)
	}
	b, err := relaylink.ParseBundle(raw)
	if err != nil {
		log.Fatal(err)
	}
	c, err := b.Client()
	if err != nil {
		log.Fatal(err)
	}
	if *addr == "" {
		if *addr, err = relayd.StreamAddrFromURL(b.MainURL); err != nil {
			log.Fatal(err)
		}
	}
	t0 := time.Now()
	sc, err := c.DialStream(context.Background(), *addr, relaylink.StreamHandlers{}, relaylink.StreamOptions{})
	if err != nil {
		log.Fatalf("connect %s: %v", *addr, err)
	}
	defer sc.Close()
	fmt.Printf("connected to %s in %v (TCP + encrypted handshake)\n", *addr, time.Since(t0).Round(time.Millisecond))
	var rawConn net.Conn
	var rawMin, rawSum time.Duration
	if *rawAddr != "" {
		if rawConn, err = net.Dial("tcp", *rawAddr); err != nil {
			log.Fatalf("raw connect: %v", err)
		}
		defer rawConn.Close()
	}
	var min, max, sum time.Duration
	for i := 0; i < *n; i++ {
		if rawConn != nil {
			buf := make([]byte, 32)
			s := time.Now()
			rawConn.Write(buf)
			io.ReadFull(rawConn, buf)
			d := time.Since(s)
			if i == 0 || d < rawMin {
				rawMin = d
			}
			rawSum += d
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s := time.Now()
		if _, err := sc.Call(ctx, relayhub.MethodEcho, []byte("ping")); err != nil {
			cancel()
			log.Fatalf("echo %d: %v", i, err)
		}
		cancel()
		d := time.Since(s)
		if i == 0 || d < min {
			min = d
		}
		if d > max {
			max = d
		}
		sum += d
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("%d encrypted echo calls: min %v  avg %v  max %v\n", *n, min.Round(10*time.Microsecond), (sum / time.Duration(*n)).Round(10*time.Microsecond), max.Round(10*time.Microsecond))
	if rawConn != nil {
		ravg := rawSum / time.Duration(*n)
		savg := sum / time.Duration(*n)
		fmt.Printf("interleaved plain TCP echo:  min %v  avg %v\n", rawMin.Round(10*time.Microsecond), ravg.Round(10*time.Microsecond))
		fmt.Printf("=> encrypted stream costs %v more than plain TCP (avg)  [%v on the 32-byte echo]\n", (savg - ravg).Round(10*time.Microsecond), (min - rawMin).Round(10*time.Microsecond))
	}
	time.Sleep(1500 * time.Millisecond)
	fmt.Printf("heartbeat RTT (smoothed): %v   best: %v\n", sc.RTT().Round(10*time.Microsecond), sc.RTTMin().Round(10*time.Microsecond))
}
