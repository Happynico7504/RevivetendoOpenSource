package main

import (
	"crypto/rand"
	"encoding/hex"
	"sync"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	"github.com/PretendoNetwork/friends-nex/database"
	"github.com/PretendoNetwork/friends-nex/globals"
	"github.com/PretendoNetwork/friends-nex/grpc"
	nexserver "github.com/PretendoNetwork/friends-nex/nex"
	"github.com/PretendoNetwork/plogger-go"
	"github.com/joho/godotenv"
)

func main() {
	godotenv.Load(".env", "../wiiu-chat-secure/.env")

	globals.Logger = plogger.NewLogger()

	b := make([]byte, 16)
	rand.Read(b)
	globals.KerberosPassword = hex.EncodeToString(b)

	globals.AuthenticationServerAccount = nex.NewAccount(types.NewPID(1), "Quazal Authentication", globals.KerberosPassword, false)
	globals.SecureServerAccount = nex.NewAccount(types.NewPID(2), "Quazal Rendez-Vous", globals.KerberosPassword, false)

	database.Connect()

	var wg sync.WaitGroup
	wg.Add(3)

	go func() { defer wg.Done(); grpc.StartServer() }()
	go func() { defer wg.Done(); nexserver.StartSecureServer() }()
	go func() { defer wg.Done(); nexserver.StartAuthenticationServer() }()

	wg.Wait()
}
