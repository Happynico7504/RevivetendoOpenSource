# Local patch to nex-protocols-common-go v1.0.17

Upstream: https://github.com/PretendoNetwork/nex-protocols-common-go (GPL-3.0, see LICENSE).

`authentication/generate_ticket.go`: the Kerberos ticket's internal data was stamped with
`DateTime(0)` (upstream has a "CHANGE THIS" comment). nex-go v2 secure servers (Wii U Chat)
reject a ticket older than two minutes ("Kerberos ticket expired"), so a relay's auth server could
never hand out a usable ticket for them. The ticket is now stamped with the current UTC time.
nex-go v1 secure servers (WSC, MK8, Badge Arcade) ignore the field, so nothing changes for them.

Used through a `replace` in relayd/go.mod; the relay's auth children are the only consumer.
