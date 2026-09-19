package authentication

// TicketForTests exposes generateTicket to relayd's tests (patch, see ../PATCHED.md): it lets them
// check what a relay's auth server actually puts into a ticket.
func TicketForTests(userPID, targetPID uint32) ([]byte, uint32) {
	return generateTicket(userPID, targetPID)
}
