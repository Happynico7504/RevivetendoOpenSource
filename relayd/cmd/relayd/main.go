//go:debug tls10server=1
//go:debug tlsrsakex=1

// relayd is the regional relay process: it syncs certificates from the main,
// terminates console TLS connections locally (same certificate selection and
// legacy TLS behaviour as the main) and forwards the requests to the main over
// the encrypted relay channel.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relayd/nexauth"
	"github.com/Happynico7504/relaylink"
)

func main() {
	// `relayd nexauth` is the NEX authentication child process (see nexauth.RunChild):
	// the same binary, so over-the-air updates cover it.
	if len(os.Args) > 1 && os.Args[1] == "nexauth" {
		nexauth.RunChild()
		return
	}
	cfgPath := flag.String("config", "/etc/relayd/relayd.json", "config file")
	showVersion := flag.Bool("version", false, "print the version and exit (also used as the OTA pre-flight check)")
	flag.Parse()
	if *showVersion {
		fmt.Printf("relayd %s\n", relayd.Version)
		return
	}

	cfg, err := relayd.LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	raw, err := os.ReadFile(cfg.Bundle)
	if err != nil {
		log.Fatalf("bundle: %v", err)
	}
	bundle, err := relaylink.ParseBundle(raw)
	if err != nil {
		log.Fatalf("bundle: %v", err)
	}
	client, err := bundle.Client()
	if err != nil {
		log.Fatalf("bundle: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Over-the-air updates: only with a pinned release key in the bundle.
	var upd *relayd.Updater
	if bundle.ReleasePublicKey != "" && !cfg.Update.Disabled {
		pub, err := relaylink.ParseReleasePublicKey(bundle.ReleasePublicKey)
		if err != nil {
			log.Fatalf("bundle release key: %v", err)
		}
		upd = &relayd.Updater{
			Client: client, PubKey: pub, Dir: cfg.DataDir, Current: relayd.CurrentVersion(),
			Window: cfg.Update.Window, Logf: log.Printf,
		}
		// A freshly installed version that keeps failing to become healthy is
		// rolled back here; exiting lets the supervisor start the restored one.
		if rolledBack, err := upd.OnStart(); err != nil {
			log.Printf("update state: %v", err)
		} else if rolledBack {
			log.Printf("rolled back a failing update: exiting so the previous version starts")
			os.Exit(0)
		}
	} else {
		log.Printf("over-the-air updates are off (no release key in the bundle, or disabled in the config)")
	}
	log.Printf("relayd version %s", relayd.Version)

	certs := &relayd.CertSet{Client: client, Dir: filepath.Join(cfg.DataDir, "certs"), Logf: log.Printf}
	if err := certs.LoadFromDisk(); err == nil {
		log.Printf("loaded %d certificates from disk", certs.Count())
	}
	// A relay without certificates cannot serve anyone: keep trying until the
	// first sync works (or we already had certificates from a previous run).
	for certs.Count() == 0 {
		if err := certs.Sync(ctx); err != nil {
			log.Printf("waiting for certificates from the main: %v", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}
	log.Printf("relay %q ready with %d certificates", bundle.RelayID, certs.Count())
	go certs.Run(ctx, cfg.CertSyncEvery())

	// Console-content cache (Miiverse): invalidations come from the main both by
	// poll and, instantly, over the stream.
	var content *relayd.ContentCache
	var fetcher *relaylink.Fetcher
	if cfg.ContentCache != nil && cfg.ContentCache.Enabled {
		store := &relaylink.MemoryStore{}
		fetcher = &relaylink.Fetcher{Client: client, Store: store, MaxStale: 45 * time.Second, PollInterval: 5 * time.Second, Logf: log.Printf}
		content = relayd.NewContentCache(*cfg.ContentCache, fetcher, store)
		go fetcher.Run(ctx)
		go func() {
			t := time.NewTicker(10 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					s := content.Stats()
					log.Printf("content cache: %d hits, %d misses, %d stored, %d flushed, %d bypassed; %d KB in %d entries",
						s.Hits, s.Misses, s.Stores, s.Flushes, s.Bypassed, store.Bytes()>>10, store.Len())
				case <-ctx.Done():
					return
				}
			}
		}()
		ec := content.Config()
		log.Printf("content cache ON for %d hosts (ttl %ds, %d MB budget, %d never-cache prefixes)", len(ec.Hosts), ec.TTLSeconds, ec.MaxMB, len(ec.NeverCache))
	}

	// The WSC edge child talks to the main through the stream, so its bridge exists only
	// when the edge component is configured and the stream is on.
	// healthyEdges[variant] is true while that edge child is up and proven; only then does this relay
	// offer the wsc-edge auth variant to the main, so a console is never sent to a dead edge.
	var healthyMu sync.Mutex
	healthyEdges := map[string]bool{} // variant name -> its edge is up and proven
	var rehello func()                // sends the hello again (set once the stream exists)
	var nexSup *relayd.NexSupervisor
	bridges := map[string]*relayd.WSCBridge{} // one per edge component, keyed by component name
	for _, c := range cfg.Components {
		if spec, ok := relayd.KnownEdges[c.Name]; ok && !c.Disabled && !cfg.StreamDisabled {
			bridges[c.Name] = &relayd.WSCBridge{Logf: log.Printf, Names: spec.Names}
		}
	}

	if !cfg.StreamDisabled {
		addr := cfg.StreamAddr
		if addr == "" {
			var err error
			if addr, err = relayd.StreamAddrFromURL(bundle.MainURL); err != nil {
				log.Fatalf("stream: %v", err)
			}
		}
		// NEX authentication servers (a supervised child process): the hub tells us
		// the games' configuration whenever the stream connects, pushes each
		// console's credential before sending it here, and answers our pulls.
		var stream *relayd.StreamClient
		handlers := relaylink.StreamHandlers{
			Event: func(_ *relaylink.StreamConn, topic string, body []byte) {
				for _, b := range bridges { // an RMC message for an edge player
					if topic == b.EdgeNames().Out {
						b.DeliverOut(body)
						return
					}
				}
				if topic == "invalidate" && fetcher != nil { // instant cache invalidation from the main
					var m struct {
						Epoch string   `json:"epoch"`
						Seq   int64    `json:"seq"`
						Tags  []string `json:"tags"`
					}
					if json.Unmarshal(body, &m) == nil {
						fetcher.ApplyPushed(m.Epoch, relaylink.Event{Seq: m.Seq, Tags: m.Tags})
					}
					return
				}
				log.Printf("stream: event %q", topic)
			},
		}
		// Calls from the main, by method.
		calls := map[string]func(context.Context, []byte) ([]byte, error){}
		handlers.Call = func(cctx context.Context, _ *relaylink.StreamConn, method string, body []byte) ([]byte, error) {
			if fn := calls[method]; fn != nil {
				return fn(cctx, body)
			}
			return nil, errors.New("unknown method")
		}
		if cfg.P2P != nil && cfg.P2P.Enabled {
			tun := relaylink.NewP2PTunnels(relaylink.P2PConfig{PortMin: cfg.P2P.PortMin, PortMax: cfg.P2P.PortMax, TracePackets: cfg.P2P.Trace, Logf: log.Printf})
			go tun.Run(ctx.Done())
			calls[relaylink.MethodP2POpen] = func(_ context.Context, body []byte) ([]byte, error) {
				var req relaylink.P2POpen
				if err := json.Unmarshal(body, &req); err != nil {
					return nil, err
				}
				ports, err := tun.Open(req.Key, req.Stations)
				if err != nil {
					return nil, err
				}
				return json.Marshal(relaylink.P2POpenResult{Ports: ports})
			}
			calls[relaylink.MethodP2PClose] = func(_ context.Context, body []byte) ([]byte, error) {
				var req relaylink.P2PClose
				if err := json.Unmarshal(body, &req); err != nil {
					return nil, err
				}
				tun.Close(req.Key)
				return []byte("ok"), nil
			}
			log.Printf("P2P tunnels enabled")
		}
		if len(cfg.NexAuth) > 0 {
			nexSup = &relayd.NexSupervisor{Logf: log.Printf}
			nexSup.Pull = func(pctx context.Context, game string, pid uint32) (string, error) {
				body, _ := json.Marshal(relaylink.NexCredGet{Game: game, PID: pid})
				out, err := stream.Call(pctx, relaylink.MethodCredGet, body)
				if err != nil {
					return "", err
				}
				var a relaylink.NexCredAnswer
				if err := json.Unmarshal(out, &a); err != nil {
					return "", err
				}
				return a.Password, nil
			}
			calls[relaylink.MethodCredPut] = func(cctx context.Context, body []byte) ([]byte, error) {
				var p relaylink.NexCredPut
				if err := json.Unmarshal(body, &p); err != nil {
					return nil, err
				}
				if err := nexSup.PutCred(cctx, p); err != nil {
					return nil, err
				}
				return []byte("ok"), nil
			}
			go nexSup.Run(ctx)
			log.Printf("NEX authentication enabled for %v", cfg.NexAuth)
		}
		// sendHello tells the main which auth games this relay hosts and applies the
		// configuration it answers with. The edge variant is offered only while the edge is healthy.
		sendHello := func(c *relaylink.StreamConn) {
			hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			names := append([]string(nil), cfg.NexAuth...)
			healthyMu.Lock()
			for _, v := range relaylink.EdgeVariants {
				if healthyEdges[v.Name] {
					names = append(names, v.Name)
				}
			}
			healthyMu.Unlock()
			sort.Strings(names[len(cfg.NexAuth):]) // stable order for the hub's logs
			body, _ := json.Marshal(relaylink.NexHelloRequest{Games: names})
			out, err := c.Call(hctx, relaylink.MethodNexHello, body)
			if err != nil {
				log.Printf("nex: hello failed: %v", err)
				return
			}
			var resp relaylink.NexHelloResponse
			if json.Unmarshal(out, &resp) != nil {
				log.Printf("nex: malformed hello answer")
				return
			}
			log.Printf("nex: the main configured %d of %d requested games", len(resp.Games), len(names))
			nexSup.SetGames(resp.Games)
		}
		for _, b := range bridges {
			b.Call = func(cctx context.Context, method string, body []byte) ([]byte, error) {
				return stream.Call(cctx, method, body)
			}
		}
		stream = &relayd.StreamClient{
			Client: client, Addr: addr, Logf: log.Printf, Handlers: handlers,
			OnDown: func(error) {
				for _, b := range bridges {
					b.StreamDown() // the main closes our players when the stream drops
				}
			},
			OnUp: func(c *relaylink.StreamConn) {
				// No players are connected through this relay yet; announcing the
				// (empty) set after every reconnect keeps the main's table exact.
				go c.Call(ctx, "presence.set", []byte(`{"pids":[]}`))
				if nexSup == nil {
					return
				}
				go sendHello(c)
			},
		}
		if nexSup != nil {
			rehello = func() {
				if c := stream.Conn(); c != nil {
					go sendHello(c)
				}
			}
		}
		go stream.Run(ctx)
		log.Printf("real-time stream to %s enabled", addr)
	}

	front := &relayd.Front{Cfg: cfg, Certs: certs, Client: client, Logf: log.Printf, Content: content}
	errc := make(chan error, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		l := l
		go func() {
			log.Printf("listening on %s (%s -> backend %q)", l.Listen, l.Mode, l.Backend)
			errc <- front.Serve(l)
		}()
	}
	if upd != nil {
		// Healthy once it has served for a while: that ends the rollback watch.
		go func() {
			select {
			case <-time.After(30 * time.Second):
				upd.Commit()
			case <-ctx.Done():
			}
		}()
		every := time.Duration(cfg.Update.CheckMinutes) * time.Minute
		if every <= 0 {
			every = 30 * time.Minute
		}
		first := time.Duration(cfg.Update.FirstCheckSeconds) * time.Second
		if first <= 0 {
			first = 2 * time.Minute
		}
		go upd.Run(ctx, every, first)
	}
	startComponents(ctx, cfg, client, bundle, bridges, edgeCoupling{
		nex: nexSup, healthy: func(variant string, h bool) {
			healthyMu.Lock()
			healthyEdges[variant] = h
			healthyMu.Unlock()
			if rehello != nil {
				rehello()
			}
		},
	})
	select {
	case err := <-errc:
		log.Fatalf("listener stopped: %v", err)
	case <-ctx.Done():
	}
}
