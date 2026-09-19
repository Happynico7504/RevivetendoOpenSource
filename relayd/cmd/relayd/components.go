package main

import (
	"context"
	"log"
	"path/filepath"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

// startComponents runs every configured separately shipped binary under its own
// supervisor, each kept current by its own updater. A component never affects relayd: it
// has its own directory, version line, rollback and restarts.
func startComponents(ctx context.Context, cfg *relayd.Config, client *relaylink.Client, bundle *relaylink.RelayBundle, wsc *relayd.WSCBridge) {
	for _, cc := range cfg.Components {
		cc := cc
		if cc.Disabled {
			continue
		}
		upd := &relayd.Updater{
			Component: cc.Name, Client: client, Dir: filepath.Join(cfg.DataDir, "components", cc.Name),
			Window: cfg.Update.Window, KeepRunning: true, Logf: log.Printf,
		}
		sup := &relayd.ComponentSupervisor{Updater: upd, Fallback: cc.Fallback, Args: cc.Args, Env: cc.Env, Logf: log.Printf}
		if cc.Name == "wscedge" && wsc != nil {
			// Connected to the main through the stream: the child runs in pipe mode.
			sup.Args = append(append([]string(nil), cc.Args...), "-pipe")
			sup.Pipe = wsc.Attach
		}
		upd.Exit = sup.Restart
		go sup.Run(ctx)

		if bundle.ReleasePublicKey == "" || cfg.Update.Disabled {
			log.Printf("component %s: over-the-air updates are off; it runs only from its fallback binary", cc.Name)
			continue
		}
		pub, err := relaylink.ParseReleasePublicKey(bundle.ReleasePublicKey)
		if err != nil {
			log.Printf("component %s: bundle release key: %v", cc.Name, err)
			continue
		}
		upd.PubKey = pub
		every := time.Duration(cfg.Update.CheckMinutes) * time.Minute
		if every <= 0 {
			every = 30 * time.Minute
		}
		first := time.Duration(cfg.Update.FirstCheckSeconds) * time.Second
		if first <= 0 {
			first = 2 * time.Minute
		}
		go upd.Run(ctx, every, first)
		log.Printf("component %s: supervised, updates every %v", cc.Name, every)
	}
}
