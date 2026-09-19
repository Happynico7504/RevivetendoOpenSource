package relaylink

// Shared description of the NEX authentication servers a relay can host, and the
// messages the hub and a relay exchange about them over the real-time stream.
//
// The static part mirrors the three near-identical auth servers on the main
// (wsc-authentication, mk8-authentication, badge-arcade-authentication): they
// differ only in these values. The dynamic part (where the secure server is,
// and the Kerberos password shared with it) is filled in by the main.

// Stream methods.
const (
	MethodNexHello = "nex.hello" // relay -> hub: NexHelloRequest -> NexHelloResponse
	MethodCredPut  = "cred.put"  // hub -> relay: NexCredPut (acknowledged)
	MethodCredGet  = "cred.get"  // relay -> hub: NexCredGet -> NexCredAnswer (only for consoles the hub sent there)
)

// The WSC edge auth variant.
const (
	WSCEdgeGame       = "wsc-edge" // auth server whose secure server is the relay itself
	WSCEdgeBase       = "wsc"      // the game whose configuration it is derived from
	WSCEdgeSecurePort = "60015"    // where the edge listens: the same port wsc-secure uses on the main
)

// EdgeGame derives the wsc-edge configuration from WSC's: same access key, NEX version and
// Kerberos secret (the edge decrypts the tickets the auth server issues), but consoles are
// sent to secureHost, the relay.
func EdgeGame(wsc NexGame, secureHost string) NexGame {
	g := NexGameDefaults()[WSCEdgeGame]
	g.GameServerID, g.AccessKey, g.BuildName = wsc.GameServerID, wsc.AccessKey, wsc.BuildName
	g.NEXMajor, g.NEXMinor, g.NEXPatch = wsc.NEXMajor, wsc.NEXMinor, wsc.NEXPatch
	g.KerberosPassword = wsc.KerberosPassword
	g.SecureHost, g.SecurePort = secureHost, WSCEdgeSecurePort
	return g
}

type NexGame struct {
	Name         string `json:"name"`           // "wsc", "mk8", "badge-arcade"
	GameServerID string `json:"game_server_id"` // the id consoles ask for in nex_token, e.g. 1012F100
	Port         int    `json:"port"`           // UDP port of the auth server (same on the relay as on the main)
	AccessKey    string `json:"access_key"`
	NEXMajor     int    `json:"nex_major"`
	NEXMinor     int    `json:"nex_minor"`
	NEXPatch     int    `json:"nex_patch"`
	BuildName    string `json:"build_name"`

	// Provided by the main (never hard-coded on a relay):
	SecureHost       string `json:"secure_host,omitempty"` // where consoles are sent after authenticating
	SecurePort       string `json:"secure_port,omitempty"`
	KerberosPassword string `json:"kerberos_password,omitempty"`
}

// NexGameDefaults is the static table.
func NexGameDefaults() map[string]NexGame {
	return map[string]NexGame{
		"wsc": {Name: "wsc", GameServerID: "1012F100", Port: 60014, AccessKey: "4d324052", NEXMajor: 3, NEXMinor: 4, NEXPatch: 0, BuildName: "Pretendo WSC"},
		"mk8": {Name: "mk8", GameServerID: "1010EB00", Port: 60002, AccessKey: "25dbf96a", NEXMajor: 3, NEXMinor: 5, NEXPatch: 4, BuildName: "Pretendo MK7"},
		// wsc-edge is WSC's auth server with a different secure server: the relay itself, where
		// the WSC edge terminates the session. Everything else, including the Kerberos secret,
		// is WSC's (see EdgeGame); it is derived per relay, never configured on its own.
		WSCEdgeGame: {Name: WSCEdgeGame, GameServerID: "1012F100", Port: 60114, AccessKey: "4d324052", NEXMajor: 3, NEXMinor: 4, NEXPatch: 0, BuildName: "Pretendo WSC"},
		// Wii U Chat: its auth and secure servers run in one process on the main (wiiu-chat) with
		// NEX library version 3.3.2 and this access key; the auth server listens on 60004.
		"wiiu-chat":    {Name: "wiiu-chat", GameServerID: "1005A000", Port: 60004, AccessKey: "e7a47214", NEXMajor: 3, NEXMinor: 3, NEXPatch: 2, BuildName: "Pretendo WiiU Chat Auth"},
		"badge-arcade": {Name: "badge-arcade", GameServerID: "00134600", Port: 60018, AccessKey: "82d5962d", NEXMajor: 3, NEXMinor: 7, NEXPatch: 16, BuildName: "Badge Arcade Auth"},
	}
}

type NexHelloRequest struct {
	Games []string `json:"games"` // auth servers this relay is configured to host
}

type NexHelloResponse struct {
	Games []NexGame `json:"games"` // complete configuration for each requested game the main supports
}

type NexCredPut struct {
	Game       string `json:"game"`
	PID        uint32 `json:"pid"`
	Password   string `json:"password"`
	TTLSeconds int    `json:"ttl_seconds"`
}

type NexCredGet struct {
	Game string `json:"game"`
	PID  uint32 `json:"pid"`
}

type NexCredAnswer struct {
	Password string `json:"password"`
}
