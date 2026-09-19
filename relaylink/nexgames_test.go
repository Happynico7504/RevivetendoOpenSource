package relaylink

import "testing"

func TestEdgeGameMatchesWSCWhereTheEdgeMustAndDiffersWhereItShould(t *testing.T) {
	wsc := NexGameDefaults()["wsc"]
	wsc.KerberosPassword, wsc.SecureHost, wsc.SecurePort = "the-secret", "45.157.178.35", "60015"
	e := EdgeGame(wsc, "107.173.31.124")

	// The edge terminates sessions whose tickets WSC's auth server issued, so these must match.
	if e.KerberosPassword != wsc.KerberosPassword || e.AccessKey != wsc.AccessKey || e.GameServerID != wsc.GameServerID ||
		e.NEXMajor != wsc.NEXMajor || e.NEXMinor != wsc.NEXMinor || e.NEXPatch != wsc.NEXPatch || e.BuildName != wsc.BuildName {
		t.Fatalf("edge game diverges from wsc: %+v vs %+v", e, wsc)
	}
	if e.Name != WSCEdgeGame || e.Port == wsc.Port || e.Port == 0 {
		t.Fatalf("edge needs its own name and auth port: %+v", e)
	}
	if e.SecureHost != "107.173.31.124" || e.SecurePort != WSCEdgeSecurePort {
		t.Fatalf("consoles must be sent to the relay: %s:%s", e.SecureHost, e.SecurePort)
	}
	// The edge listens on the same secure port wsc-secure uses on the main.
	if WSCEdgeSecurePort != "60015" {
		t.Fatalf("secure port %s", WSCEdgeSecurePort)
	}
	// The auth ports of all games are distinct, or two servers would fight over one socket.
	seen := map[int]string{}
	for name, g := range NexGameDefaults() {
		if other, dup := seen[g.Port]; dup {
			t.Fatalf("games %s and %s share auth port %d", name, other, g.Port)
		}
		seen[g.Port] = name
	}
}

func TestChatEdgeGameMatchesWiiUChatWhereTheEdgeMustAndDiffersWhereItShould(t *testing.T) {
	chat := NexGameDefaults()["wiiu-chat"]
	chat.KerberosPassword = "shared"
	e := DeriveEdgeGame(EdgeVariants[WUCEdgeBase], chat, "107.173.31.124")
	if e.KerberosPassword != "shared" || e.AccessKey != chat.AccessKey || e.GameServerID != chat.GameServerID ||
		e.NEXMajor != chat.NEXMajor || e.NEXMinor != chat.NEXMinor || e.NEXPatch != chat.NEXPatch || e.BuildName != chat.BuildName {
		t.Fatalf("chat edge game diverges from wiiu-chat: %+v vs %+v", e, chat)
	}
	if e.Name != WUCEdgeGame || e.Port == chat.Port || e.SecureHost != "107.173.31.124" || e.SecurePort != "60005" {
		t.Fatalf("chat edge routing: %+v", e)
	}
	// Both edge variants exist and none collides with another game (auth ports are checked elsewhere).
	if len(EdgeVariants) != 2 || EdgeVariants["wsc"].Name != WSCEdgeGame || EdgeVariants["wiiu-chat"].Name != WUCEdgeGame {
		t.Fatalf("edge variants: %+v", EdgeVariants)
	}
}

func TestEdgeNamesDoNotCollide(t *testing.T) {
	seen := map[string]string{}
	for who, n := range map[string]EdgeNames{"wsc": WSCEdgeNames, "wuc": WUCEdgeNames} {
		for _, name := range []string{n.Open, n.RMC, n.Alive, n.Close, n.Stats, n.Trace, n.Out} {
			if other, dup := seen[name]; dup {
				t.Fatalf("%q used by both %s and %s", name, who, other)
			}
			seen[name] = who
		}
	}
	// WSC keeps the names relays and hubs already speak.
	if WSCEdgeNames.Open != MethodWSCOpen || WSCEdgeNames.RMC != MethodWSCRMC || WSCEdgeNames.Out != TopicWSCOut ||
		WSCEdgeNames.Stats != MethodWSCStats || WSCEdgeNames.Trace != MethodWSCTrace || WSCEdgeNames.Close != MethodWSCClose || WSCEdgeNames.Alive != MethodWSCAlive {
		t.Fatalf("WSC names changed: %+v", WSCEdgeNames)
	}
}
