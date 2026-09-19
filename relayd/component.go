package relayd

import (
	"context"
	"path/filepath"

	"github.com/Happynico7504/relaylink"
)

// Component is one separately shipped binary: its updater and the supervisor that runs it, wired
// together. The wiring is the point of this type: an installed update must restart only the CHILD,
// never relayd itself (the Updater's default, when nothing is set, is to exit the process), and the
// supervisor must actually be started. Both were lost once in a refactor of cmd/relayd that no test
// could see (relayd then exited after every component update and ran no component at all), so they
// live here, where tests cover them.
type Component struct {
	Updater    *Updater
	Supervisor *ComponentSupervisor
}

// NewComponent builds a component's updater and supervisor for the given configuration.
func NewComponent(cc ComponentConfig, client *relaylink.Client, dataDir, window string, logf func(string, ...any)) *Component {
	upd := &Updater{
		Component: cc.Name, Client: client, Dir: filepath.Join(dataDir, "components", cc.Name),
		Window: window, KeepRunning: true, Logf: logf,
	}
	sup := &ComponentSupervisor{Updater: upd, Fallback: cc.Fallback, Args: cc.Args, Env: cc.Env, Logf: logf}
	upd.Exit = sup.Restart // an installed update restarts this component only
	return &Component{Updater: upd, Supervisor: sup}
}

// Start runs the supervisor (and so the child) until ctx ends. Updates are checked separately, by
// Updater.Run, once a release key is known.
func (c *Component) Start(ctx context.Context) {
	go c.Supervisor.Run(ctx)
}
