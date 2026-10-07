package nex_shop_nintendo_badge_arcade

import (
	"crypto/rand"
	"encoding/hex"
	"log"

	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"

	"github.com/PretendoNetwork/nex-go"
	nexproto "github.com/PretendoNetwork/nex-protocols-go/shop/nintendo-badge-arcade"
)

// 2026-09-23: was stubbed (always an empty string). Across every observed real
// purchase attempt tonight, this is the *last* NEX call the console ever makes
// before the connection goes idle and gets kicked - nothing (no follow-up NEX
// call, no ninja purchase-commit HTTP call) ever happens after it. mint's own
// code.bin has a "rivToken" JSON field name sitting alongside real billing
// fields (card_number/limit_type/limit_value/amount) for what looks like a
// purchase-commit request we've never actually observed being sent - the
// leading theory is mint won't proceed to that step with an empty token.
// No existing reference implementation reverse-engineers a real token format
// (checked account-proxy's own conventions and the one other public Badge
// Arcade server project); trying a plausible non-empty, random-looking token
// first, matching this project's existing token style (see JSESSIONID/
// badge_arcade_token in account-proxy), to see whether mint proceeds further
// at all rather than continuing to guess a cryptographically "real" format.
func GetRivToken(err error, client *nex.Client, callID uint32, itemCode string, referenceID []byte) {
	rmcResponseStream := nex.NewStreamOut(globals.NEXServer)

	tokenBytes := make([]byte, 16)
	rand.Read(tokenBytes)
	token := hex.EncodeToString(tokenBytes)
	log.Printf("GetRivToken: itemCode=%q referenceID=%x -> token=%s", itemCode, referenceID, token)

	rmcResponseStream.WriteString(token)

	rmcResponseBody := rmcResponseStream.Bytes()

	rmcResponse := nex.NewRMCResponse(nexproto.ProtocolID, callID)
	rmcResponse.SetSuccess(nexproto.MethodGetRivToken, rmcResponseBody)
	rmcResponse.SetCustomID(nexproto.CustomProtocolID)

	rmcResponseBytes := rmcResponse.Bytes()

	responsePacket, _ := nex.NewPacketV1(client, nil)

	responsePacket.SetVersion(1)
	responsePacket.SetSource(0xA1)
	responsePacket.SetDestination(0xAF)
	responsePacket.SetType(nex.DataPacket)
	responsePacket.SetPayload(rmcResponseBytes)

	responsePacket.AddFlag(nex.FlagNeedsAck)
	responsePacket.AddFlag(nex.FlagReliable)

	globals.NEXServer.Send(responsePacket)
}
