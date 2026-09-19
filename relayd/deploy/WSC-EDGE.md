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
- Sends are diverted inside nex-go (`Client.SetOutHook`), not in `sendResponse`: that also covers
  the protocol library's own replies. (Original plan was to patch the two send sites; see "As built".)
- Only 4 places read `client.Address()` (station URLs, ping, traceroute): they work
  unchanged with the virtual client's address.

## As built (milestones 1-2)

```
console --PRUDP/UDP--> wscedge (child of relayd, relay)
   wscedge <--JSON lines, fds 3/4--> relayd  (WSCBridge)
   relayd  <--real-time stream, wsc.open/rmc/alive/close calls, wsc.out events--> relayhub (EdgeBridge)
   relayhub <--loopback HTTP--> wsc-secure  (edge.go, port 9451; hub gets /edge/out on 9401)
```

- The relay verifies the Kerberos ticket and answers acks/keepalives itself. The main never sees
  them; the edge reports liveness (`wsc.alive`, every 2 s) so the existing stale/hole-punch/match
  grace logic on the main keeps working unchanged.
- The main creates a virtual `*nex.Client` per player. nex-go patch (see RevivetendoVendorPatches):
  `Client.SetOutHook` diverts `Server.Send`, so the protocol library's own replies reach the player
  as well as wsc-secure's; `RegisterClient` lets NAT-traversal lookups find edge players;
  `Packet.SetRMCRequest` injects the relay's request into `Emit("Data")`, so every existing
  handler runs unmodified.
- Responses are asynchronous: a call returns as soon as the main accepts it, the answer comes back
  as `wsc.out`. A refused `open` means the Connect is not acknowledged (console sees a failed
  connection, as if the server were down).
- Failure handling: stream lost -> the hub closes every player of that relay on the main and the
  relay resets the edge's sessions (consoles reconnect); hub restart -> the main's janitor releases
  orphaned edge sessions; a relay may only touch its own players (the hub fills in the relay id
  from the authenticated stream and wsc-secure checks ownership).
- Extended protocol ids (0x7f + custom id, e.g. WSC's club protocol 0x83) are forwarded intact.
- Off by default on the main: `wsc-secure/edge.enabled` (or WSC_EDGE=1). The edge runs on the relay
  only if the relay's config lists the `wscedge` component. Verified live with `edge_probe.py`
  (forged tickets, fake PID 1999000001).

## Milestones
1. DONE. Relay terminates PRUDP and echoes RMC (proved with the NintendoClients python client).
2. DONE (calls + failure handling). Main-side virtual client + bridge. Still to try: a real Register/matchmaking flow (needs a client that speaks the game protocol, not just single calls).
3. Server-initiated pushes (matchmaking notifications) to edge players.
4. Probes on the relay + reporting; compare RTT/stability edge vs direct.
5. Only then: decide about production routing (NEX auth hands out the relay as secure server).

## Risks
- Server-initiated RMC/notifications need the stream to be low-latency and ordered per pid.
- Player reconnects: stale edge sessions on the main must be dropped like stale `currentClient`.
- nex-go retransmit/timeouts are tuned for a local socket; verify on the relay.
