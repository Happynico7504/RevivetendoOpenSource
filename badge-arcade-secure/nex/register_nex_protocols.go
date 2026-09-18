package nex

import (
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"
	nex_datastore "github.com/PretendoNetwork/nintendo-badge-arcade-secure/nex/datastore"
	nex_datastore_nintendo_badge_arcade "github.com/PretendoNetwork/nintendo-badge-arcade-secure/nex/datastore/nintendo-badge-arcade"
	nex_secure_connection "github.com/PretendoNetwork/nintendo-badge-arcade-secure/nex/secure-connection"
	nex_secure_connection_nintendo_badge_arcade "github.com/PretendoNetwork/nintendo-badge-arcade-secure/nex/secure-connection/nintendo-badge-arcade"
	nex_shop_nintendo_badge_arcade "github.com/PretendoNetwork/nintendo-badge-arcade-secure/nex/shop/nintendo-badge-arcade"
	"github.com/PretendoNetwork/nex-go"
	datastore_nintendo_badge_arcade "github.com/PretendoNetwork/nex-protocols-go/datastore/nintendo-badge-arcade"
	secure_connection_nintendo_badge_arcade "github.com/PretendoNetwork/nex-protocols-go/secure-connection/nintendo-badge-arcade"
	shop_nintendo_badge_arcade "github.com/PretendoNetwork/nex-protocols-go/shop/nintendo-badge-arcade"
)

func registerNEXProtocols() {

	secureConnectionProtocol := secure_connection_nintendo_badge_arcade.NewSecureConnectionNintendoBadgeArcadeProtocol(globals.NEXServer)

	secureConnectionProtocol.Register(nex_secure_connection.Register)
	secureConnectionProtocol.GetMaintenanceStatus(nex_secure_connection_nintendo_badge_arcade.GetMaintenanceStatus)

	dataStoreNintendoBadgeArcadeProtocol := datastore_nintendo_badge_arcade.NewDataStoreNintendoBadgeArcadeProtocol(globals.NEXServer)

	dataStoreNintendoBadgeArcadeProtocol.GetPersistenceInfo(nex_datastore.GetPersistenceInfo)
	dataStoreNintendoBadgeArcadeProtocol.PostMetaBinary(nex_datastore.PostMetaBinary)
	dataStoreNintendoBadgeArcadeProtocol.PreparePostObject(nex_datastore.PreparePostObject)
	dataStoreNintendoBadgeArcadeProtocol.CompletePostObject(nex_datastore.CompletePostObject)
	dataStoreNintendoBadgeArcadeProtocol.PrepareGetObject(nex_datastore.PrepareGetObject)
	dataStoreNintendoBadgeArcadeProtocol.GetMetaByOwnerID(nex_datastore_nintendo_badge_arcade.GetMetaByOwnerID)
	dataStoreNintendoBadgeArcadeProtocol.ChangeMeta(nex_datastore.ChangeMeta)
	dataStoreNintendoBadgeArcadeProtocol.PrepareUpdateObject(nex_datastore.PrepareUpdateObject)
	dataStoreNintendoBadgeArcadeProtocol.CompleteUpdateObject(nex_datastore.CompleteUpdateObject)

	// Not using shop_nintendo_badge_arcade.NewShopNintendoBadgeArcadeProtocol here:
	// its Setup() wires up a dispatch switch with a case for MethodPostPlayLog but
	// none for MethodGetRivToken, so a real GetRivToken call always falls through
	// to RespondNotImplementedCustom even though a handler is registered below.
	// Construct the protocol manually and dispatch both methods ourselves instead.
	shopNintendoBadgeArcadePrococol := &shop_nintendo_badge_arcade.ShopNintendoBadgeArcadeProtocol{Server: globals.NEXServer}
	shopNintendoBadgeArcadePrococol.ShopProtocol.Server = globals.NEXServer

	shopNintendoBadgeArcadePrococol.PostPlayLog(nex_shop_nintendo_badge_arcade.PostPlayLog)
	shopNintendoBadgeArcadePrococol.GetRivToken(nex_shop_nintendo_badge_arcade.GetRivToken)

	globals.NEXServer.On("Data", func(packet nex.PacketInterface) {
		request := packet.RMCRequest()

		if request.ProtocolID() != shop_nintendo_badge_arcade.ProtocolID || request.CustomID() != shop_nintendo_badge_arcade.CustomProtocolID {
			return
		}

		switch request.MethodID() {
		case shop_nintendo_badge_arcade.MethodGetRivToken:
			shopNintendoBadgeArcadePrococol.HandleGetRivToken(packet)
		case shop_nintendo_badge_arcade.MethodPostPlayLog:
			shopNintendoBadgeArcadePrococol.HandlePostPlayLog(packet)
		}
	})
}
