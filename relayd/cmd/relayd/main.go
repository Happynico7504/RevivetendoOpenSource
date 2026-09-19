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
	flag.Parse()

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

	front := &relayd.Front{Cfg: cfg, Certs: certs, Client: client, Logf: log.Printf}
	errc := make(chan error, len(cfg.Listeners))
	for _, l := range cfg.Listeners {
		l := l
		go func() {
			log.Printf("listening on %s (%s -> backend %q)", l.Listen, l.Mode, l.Backend)
			errc <- front.Serve(l)
		}()
	}
	select {
	case err := <-errc:
		log.Fatalf("listener stopped: %v", err)
	case <-ctx.Done():
	}
}
