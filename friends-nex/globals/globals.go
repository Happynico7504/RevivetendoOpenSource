package globals

import (
	"sync"

	nex "github.com/PretendoNetwork/nex-go/v2"
	common_ticket_granting "github.com/PretendoNetwork/nex-protocols-common-go/v2/ticket-granting"
	"github.com/PretendoNetwork/plogger-go"
)

var Logger *plogger.Logger

var KerberosPassword string

var AuthenticationServer *nex.PRUDPServer
var AuthenticationEndpoint *nex.PRUDPEndPoint
var SecureServer *nex.PRUDPServer
var SecureEndpoint *nex.PRUDPEndPoint

var AuthenticationServerAccount *nex.Account
var SecureServerAccount *nex.Account

var CommonAuthProtocol *common_ticket_granting.CommonProtocol

// ConnectionInfo holds the connection needed to send server-initiated packets.
type ConnectionInfo struct {
	Connection  *nex.PRUDPConnection
	SubstreamID uint8
}

// ConnectedClients maps PID (uint64) → *ConnectionInfo.
var ConnectedClients sync.Map

var sessionPasswordsMu sync.RWMutex
var sessionPasswords = map[uint64]string{}

func SetSessionPassword(pid uint64, password string) {
	sessionPasswordsMu.Lock()
	sessionPasswords[pid] = password
	sessionPasswordsMu.Unlock()
}

func GetSessionPassword(pid uint64) (string, bool) {
	sessionPasswordsMu.RLock()
	p, ok := sessionPasswords[pid]
	sessionPasswordsMu.RUnlock()
	return p, ok
}
