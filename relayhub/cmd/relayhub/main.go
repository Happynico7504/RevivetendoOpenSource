// relayhub is the main instance's side of the relay network: it serves the
// encrypted relay API, manages the relay registry, and generates the DNS
// server's region config from it.
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/lib/pq"
	"github.com/redis/go-redis/v9"

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
	fmt.Printf("wrote %s (0600)\npublic key:\n%s", *out, pub)
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
	keyPath := fs.String("key", defaultKeyPath(), "main RSA private key")
	envFile := fs.String("env", "../wiiu-chat-secure/.env", "env file with PN_WUC_POSTGRES_URI")
	redisAddr := fs.String("redis", "127.0.0.1:6379", "Redis for replay protection")
	fs.Parse(args)

	db := openDB(*envFile)
	reg := &relayhub.PGRegistry{DB: db}
	if err := reg.Init(context.Background()); err != nil {
		log.Fatalf("registry schema: %v", err)
	}
	invlog := relayhub.NewInvalidationLog(10000)
	hub := &relayhub.Hub{Src: &relayhub.PGSource{DB: db}, Log: invlog}
	keys := &relayhub.KeyLookup{Reg: reg}
	srv := &relaylink.Server{
		Priv:     loadKey(*keyPath),
		RelayKey: keys.Lookup,
		Replay:   &relayhub.RedisReplay{Client: redis.NewClient(&redis.Options{Addr: *redisAddr})},
	}

	// Local-only: the other services tell the hub that data changed.
	go func() {
		mux := http.NewServeMux()
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
