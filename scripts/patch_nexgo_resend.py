#!/usr/bin/env python3
"""Apply the "never decipher a reliable data packet twice" fix to a nex-go v1 copy.

Usage: patch_nexgo_resend.py <nex-go dir> [...]
       (e.g. badge-arcade-secure/vendor/github.com/PretendoNetwork/nex-go)

The same change as commit f0c378c (wsc-secure vendor, relayd/wscedge/nexgo). nex-go v1
deciphered every reliable data packet it received, including a copy the client resent
because the acknowledgement was late. RC4 is one keystream per connection, so the copy
moved the server's keystream ahead of the client's for good: every later request
deciphered to garbage and was dropped unacknowledged, and the console reported a lost
connection a minute or so later. Requires the "signature before decipher" patch
(RevivetendoVendorPatches) to be applied already. Idempotent: an already patched copy
is left alone.
"""
import sys
from pathlib import Path


def patch(d: Path) -> None:
    client, pv1, server = d / "client.go", d / "packet_v1.go", d / "server.go"
    c, p, s = client.read_text(), pv1.read_text(), server.read_text()
    if "inSeqKnown" in p:
        print(f"{d}: already patched")
        return
    if "Verify signature BEFORE deciphering" not in p:
        sys.exit(f"{d}: the signature-before-decipher patch is missing; apply it first")

    field = "\tdecipher                  *rc4.Cipher\n"
    assert field in c, f"{client}: decipher field not found"
    c = c.replace(field, field +
                  "\tinSeqNext                 uint16 // sequence ID of the next reliable data packet to decipher\n"
                  "\tinSeqKnown                bool   // inSeqNext is set (false until the first one after a key change)\n", 1)
    old = "\tdecipher, _ := rc4.NewCipher(key)\n\tclient.decipher = decipher\n}"
    assert old in c, f"{client}: UpdateRC4Key not found"
    c = c.replace(old, "\tdecipher, _ := rc4.NewCipher(key)\n\tclient.decipher = decipher\n"
                       "\tclient.inSeqKnown = false // a fresh keystream: the next reliable data packet sets the order\n}", 1)

    old = "type PacketV1 struct {\n\tPacket\n"
    assert old in p, f"{pv1}: PacketV1 struct not found"
    p = p.replace(old, old + "\tduplicate                 bool // a reliable data packet already processed (resent by the client): acknowledge, do not process\n", 1)
    old = ("\t\tclient := packet.Sender()\n\t\tclient.mu.Lock()\n"
           "\t\tclient.Decipher().XORKeyStream(ciphered, payloadCrypted)\n\t\tclient.mu.Unlock()\n")
    assert old in p, f"{pv1}: decipher block not found"
    p = p.replace(old, """\t\tclient := packet.Sender()
\t\tclient.mu.Lock()
\t\t// The payload is RC4 with one keystream per connection, so reliable packets must be
\t\t// deciphered exactly once each, in sequence order. A client resends a packet whose
\t\t// acknowledgement it did not get in time; deciphering that copy again moved the
\t\t// keystream ahead of the client's for good (every later packet deciphered to garbage
\t\t// and was dropped unacknowledged). A copy of an earlier packet is acknowledged but not
\t\t// deciphered; one from beyond a gap waits (unacknowledged, so it is resent).
\t\treliable := packet.HasFlag(FlagReliable)
\t\tif reliable && client.inSeqKnown {
\t\t\tif ahead := int16(packet.SequenceID() - client.inSeqNext); ahead < 0 {
\t\t\t\tclient.mu.Unlock()
\t\t\t\tpacket.duplicate = true
\t\t\t\treturn nil
\t\t\t} else if ahead > 0 {
\t\t\t\tclient.mu.Unlock()
\t\t\t\treturn errors.New("[PRUDPv1] reliable data packet ahead of a missing one, waiting for it")
\t\t\t}
\t\t}
\t\tclient.Decipher().XORKeyStream(ciphered, payloadCrypted)
\t\tif reliable {
\t\t\tclient.inSeqNext = packet.SequenceID() + 1
\t\t\tclient.inSeqKnown = true
\t\t}
\t\tclient.mu.Unlock()
""", 1)
    p += """
// Duplicate reports a reliable data packet the client resent after it had already been
// processed. It is acknowledged again, but not deciphered or handled a second time.
func (packet *PacketV1) Duplicate() bool { return packet.duplicate }
"""
    old = "\tcase DataPacket:\n\t\tserver.Emit(\"Data\", packet)"
    assert old in s, f"{server}: DataPacket dispatch not found"
    s = s.replace(old, """\tcase DataPacket:
\t\tif p1, ok := packet.(*PacketV1); ok && p1.Duplicate() {
\t\t\tbreak // already handled; the acknowledgement above is what the client was missing
\t\t}
\t\tserver.Emit("Data", packet)""", 1)

    client.write_text(c)
    pv1.write_text(p)
    server.write_text(s)
    print(f"{d}: patched")


for arg in sys.argv[1:]:
    patch(Path(arg))
