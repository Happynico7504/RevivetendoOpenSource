package globals

import (
	"strconv"

	nex "github.com/PretendoNetwork/nex-go/v2"
	"github.com/PretendoNetwork/nex-go/v2/types"
	"github.com/PretendoNetwork/friends-nex/database"
)

func AccountDetailsByPID(pid types.PID) (*nex.Account, *nex.Error) {
	if pid.Equals(AuthenticationServerAccount.PID) {
		return AuthenticationServerAccount, nil
	}
	if pid.Equals(SecureServerAccount.PID) {
		return SecureServerAccount, nil
	}
	if pw, ok := GetSessionPassword(uint64(pid)); ok {
		return nex.NewAccount(pid, strconv.FormatUint(uint64(pid), 10), pw, false), nil
	}
	if pw, ok := database.GetNEXPassword(uint64(pid)); ok {
		return nex.NewAccount(pid, strconv.FormatUint(uint64(pid), 10), pw, false), nil
	}
	return nil, nex.NewError(nex.ResultCodes.Authentication.Unknown, "no account for PID")
}

func AccountDetailsByUsername(username string) (*nex.Account, *nex.Error) {
	if username == AuthenticationServerAccount.Username {
		return AuthenticationServerAccount, nil
	}
	if username == SecureServerAccount.Username {
		return SecureServerAccount, nil
	}
	pidInt, err := strconv.ParseUint(username, 10, 64)
	if err != nil {
		return nil, nex.NewError(nex.ResultCodes.RendezVous.InvalidUsername, "invalid username")
	}
	pid := types.NewPID(pidInt)
	if pw, ok := GetSessionPassword(pidInt); ok {
		return nex.NewAccount(pid, username, pw, false), nil
	}
	if pw, ok := database.GetNEXPassword(pidInt); ok {
		return nex.NewAccount(pid, username, pw, false), nil
	}
	return nil, nex.NewError(nex.ResultCodes.Authentication.Unknown, "no account for username")
}
