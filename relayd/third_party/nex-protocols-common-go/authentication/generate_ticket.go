package authentication

import (
	"crypto/rand"

	"github.com/PretendoNetwork/nex-go"
)

func generateTicket(userPID uint32, targetPID uint32) ([]byte, uint32) {
	if commonAuthenticationProtocol.passwordFromPIDHandler == nil {
		logger.Warning("Missing passwordFromPIDHandler!")
		return []byte{}, nex.Errors.Core.Unknown
	}

	var userPassword string
	var targetPassword string
	var errorCode uint32

	// TODO: Maybe we should error out if the user PID is the server account?
	switch userPID {
	case 2: // "Quazal Rendez-Vous" (the server user) account
		userPassword = commonAuthenticationProtocol.server.KerberosPassword()
	case 100: // guest user account
		userPassword = "MMQea3n!fsik"
	default:
		userPassword, errorCode = commonAuthenticationProtocol.passwordFromPIDHandler(userPID)
	}

	if errorCode != 0 {
		return []byte{}, errorCode
	}

	switch targetPID {
	case 2: // "Quazal Rendez-Vous" (the server user) account
		targetPassword = commonAuthenticationProtocol.server.KerberosPassword()
	case 100: // guest user account
		targetPassword = "MMQea3n!fsik"
	default:
		targetPassword, errorCode = commonAuthenticationProtocol.passwordFromPIDHandler(userPID)
	}

	if errorCode != 0 {
		return []byte{}, errorCode
	}

	userKey := nex.DeriveKerberosKey(userPID, []byte(userPassword))
	targetKey := nex.DeriveKerberosKey(targetPID, []byte(targetPassword))
	sessionKey := make([]byte, commonAuthenticationProtocol.server.KerberosKeySize())
	rand.Read(sessionKey)

	ticketInternalData := nex.NewKerberosTicketInternalData()
	// Patched (upstream stamps every ticket with DateTime 0 and a "CHANGE THIS" comment). Servers
	// built on nex-go v2 (Wii U Chat) accept a ticket for only two minutes after it was issued and
	// so reject a zero date as "Kerberos ticket expired"; nex-go v1 servers never look at it.
	ticketInternalData.SetTimestamp(nex.NewDateTime(nex.NewDateTime(0).UTC()))
	ticketInternalData.SetUserPID(userPID)
	ticketInternalData.SetSessionKey(sessionKey)
	encryptedTicketInternalData := ticketInternalData.Encrypt(targetKey, nex.NewStreamOut(commonAuthenticationProtocol.server))

	ticket := nex.NewKerberosTicket()
	ticket.SetSessionKey(sessionKey)
	ticket.SetTargetPID(targetPID)
	ticket.SetInternalData(encryptedTicketInternalData)

	return ticket.Encrypt(userKey, nex.NewStreamOut(commonAuthenticationProtocol.server)), 0
}
