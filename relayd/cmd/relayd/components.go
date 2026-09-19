package main

import (
	"context"
	"log"
	"time"

	"github.com/Happynico7504/relayd"
	"github.com/Happynico7504/relaylink"
)

// edgeCoupling connects the WSC edge to the rest of the relay: the real Kerberos secret comes
// from the NEX auth configuration the main sends, and the edge's health decides whether this
// relay offers the wsc-edge auth variant.
type edgeCoupling struct {
	nex     *relayd.NexSupervisor // nil if this relay hosts no NEX auth servers
	healthy func(variant string, healthy bool)
}

// startComponents runs every configured separately shipped binary under its own
// supervisor, each kept current by its own updater. A component never affects relayd: it
// has its own directory, version line, rollback and restarts.
func startComponents(ctx context.Context, cfg *relayd.Config, client *relaylink.Client, bundle *relaylink.RelayBundle, bridges map[string]*relayd.WSCBridge, edge edgeCoupling) {
	for _, cc := range cfg.Components {
		cc := cc
		if cc.Disabled {
			continue
		}
		comp := relayd.NewComponent(cc, client, cfg.DataDir, cfg.Update.Window, log.Printf)
		upd, sup := comp.Updater, comp.Supervisor
		if spec, isEdge := relayd.KnownEdges[cc.Name]; isEdge && bridges[cc.Name] != nil {
			// Connected to the main through the stream: the child runs in pipe mode.
			sup.Args = append(append([]string(nil), cc.Args...), "-pipe")
			sup.Pipe = bridges[cc.Name].Attach
			variant := spec.Variant.Name
			sup.OnHealthy = func(h bool) { edge.healthy(variant, h) }
			if edge.nex != nil {
				// The edge decrypts the tickets its game's auth server issues, so it needs that
				// game's current secret. The secret changes whenever the bridge restarts, so it is
				// read at each launch, and the edge is relaunched when it changes.
				secret := func() string {
					g, _ := edge.nex.Game(spec.Variant.Base)
					return g.KerberosPassword
				}
				sup.Ready = func() bool { return secret() != "" }
				sup.EnvFunc = func() []string { return []string{spec.SecretEnv + "=" + secret()} }
				last := secret()
				edge.nex.AddOnGames(func([]relaylink.NexGame) {
					if now := secret(); now != last {
						last = now
						sup.Restart()
					}
				})
			}
		}
		comp.Start(ctx) // an update restarts only this child (NewComponent); see relayd/component.go
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
