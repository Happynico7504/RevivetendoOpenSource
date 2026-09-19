# WSC edge proxy (step 3 prototype) — design

Goal: a player's WSC PRUDP session terminates on the nearest relay; game logic stays on
the main. Prototype only: separate UDP port, never on the production path until proven.

## Why terminate instead of forwarding datagrams
A raw UDP forwarder adds a hop and hides the player's address. Terminating PRUDP on the
relay makes acks, retransmits, keepalive pings and the Kerberos handshake local (~few ms
to the player); only complete RMC calls cross the ocean, over the existing real-time
stream. Note: WSC gameplay is peer-to-peer; the secure server only does matchmaking,
ranking and datastore, so the gain is responsiveness of those calls and connection
stability, not in-game latency.

## Split
Relay (child process `relayd wscedge`, like `nexauth`; nex-go panics inside its own goroutines):
- nex-go v1.0.16 server on a test UDP port, same settings as wsc-secure (PRUDP v1 minor 3,
  NEX 3.4.0, access key `4d324052`, Kerberos password from the bundle's WSC secret).
- Connect: decrypt the ticket, derive the session key, verify the check value, ack. Reuse
  wsc-secure's exact Connect logic. Tell the main: `edge.open {pid, ip, port}`.
- Data: forward `{pid, callID, proto, method, params}` to the main over a stream Call
  `wsc.rmc`; write the returned RMC response back through nex-go (relay encrypts).
- Disconnect / stale: `edge.close {pid}`.
- Connectivity probes (ICMP + PRUDP keepalive ack RTT) run HERE, since this host receives
  the player's UDP; results go to the main as an Event so `PlayerPing` lines land in one log.

Main (wsc-secure, inert unless `WSC_EDGE=1`):
- On `edge.open`: create a virtual `*nex.Client` (Address = the player's public ip:port as
  seen by the relay, PID set), register it in connectedPIDs/currentClient as usual.
- On `wsc.rmc`: build the PacketV1 the existing "Data" handler expects and run it.
- `sendResponse` (main.go:1533) and the notification send (main.go:2287) check for an edge
  session and return the RMC bytes over the stream instead of `nexServer.Send`.
- Only 4 places read `client.Address()` (station URLs, ping, traceroute): they work
  unchanged with the virtual client's address.

## Milestones
1. Relay terminates PRUDP for a test client and echoes RMC (no main logic). Prove Connect/acks/keepalive with the NintendoClients python client.
2. Main-side virtual client + `wsc.rmc` bridge; a python client completes Register/matchmaking through the edge.
3. Server-initiated pushes (matchmaking notifications) to edge players.
4. Probes on the relay + reporting; compare RTT/stability edge vs direct.
5. Only then: decide about production routing (NEX auth hands out the relay as secure server).

## Risks
- Server-initiated RMC/notifications need the stream to be low-latency and ordered per pid.
- Player reconnects: stale edge sessions on the main must be dropped like stale `currentClient`.
- nex-go retransmit/timeouts are tuned for a local socket; verify on the relay.
