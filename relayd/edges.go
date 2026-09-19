package relayd

import "github.com/Happynico7504/relaylink"

// EdgeSpec describes one edge component: the game it terminates, the stream names it uses and
// where its Kerberos secret comes from.
type EdgeSpec struct {
	Variant   relaylink.EdgeVariant // the auth variant that hands consoles to it
	Names     relaylink.EdgeNames   // its stream methods and topic
	SecretEnv string                // the environment variable its child reads the shared secret from
}

// KnownEdges maps a component name to its EdgeSpec. A component not listed here is an ordinary
// supervised binary with no game bridge.
var KnownEdges = map[string]EdgeSpec{
	"wscedge": {
		Variant:   relaylink.EdgeVariants[relaylink.WSCEdgeBase],
		Names:     relaylink.WSCEdgeNames,
		SecretEnv: "WSC_KERBEROS_PASSWORD",
	},
	"wiiuchatedge": {
		Variant:   relaylink.EdgeVariants[relaylink.WUCEdgeBase],
		Names:     relaylink.WUCEdgeNames,
		SecretEnv: "PN_WUC_KERBEROS_PASSWORD",
	},
}
