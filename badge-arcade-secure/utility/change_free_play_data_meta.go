package utility

import (
	"github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nintendo-badge-arcade-secure/database"
)

func ChangeFreePlayDataMeta(dataID uint64, metaBinary []byte) {
	dateTime := nex.NewDateTime(0)
	updatedTime := dateTime.Now()

	database.UpdateFreePlayDataMetaBinary(uint32(dataID), metaBinary, updatedTime)
}
