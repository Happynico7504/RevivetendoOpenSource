package database

import (
	"context"

	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/globals"
	"go.mongodb.org/mongo-driver/bson"
)

func UpdatePlayerSessionPort(pid uint32, port string) {
	_, err := sessionsCollection.UpdateOne(context.TODO(), bson.D{{"pid", pid}}, bson.D{{"$set", bson.D{{"port", port}}}})
	if err != nil {
		globals.Logger.Error(err.Error())
	}
}
