//go:debug tls10server=1
//go:debug tlsrsakex=1

// relayd is the regional relay process: it syncs certificates from the main,
// terminates console TLS connections locally (same certificate selection and
// legacy TLS behaviour as the main) and forwards the requests to the main over
// the encrypted relay channel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

func main() {
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

	if !cfg.StreamDisabled {
		addr := cfg.StreamAddr
		if addr == "" {
			var err error
			if addr, err = relayd.StreamAddrFromURL(bundle.MainURL); err != nil {
				log.Fatalf("stream: %v", err)
			}
		}
		stream := &relayd.StreamClient{
			Client: client, Addr: addr, Logf: log.Printf,
			Handlers: relaylink.StreamHandlers{
				Event: func(_ *relaylink.StreamConn, topic string, _ []byte) { log.Printf("stream: event %q", topic) },
			},
			OnUp: func(c *relaylink.StreamConn) {
				// No players are connected through this relay yet; announcing the
				// (empty) set after every reconnect keeps the main's table exact.
				go c.Call(ctx, "presence.set", []byte(`{"pids":[]}`))
			},
		}
		go stream.Run(ctx)
		log.Printf("real-time stream to %s enabled", addr)
	}

	front := &relayd.Front{Cfg: cfg, Certs: certs, Client: client, Logf: log.Printf}
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
	select {
	case err := <-errc:
		log.Fatalf("listener stopped: %v", err)
	case <-ctx.Done():
	}
}
