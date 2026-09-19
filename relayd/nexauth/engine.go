package nexauth

import (
	"errors"
	"fmt"
	"time"

	nex "github.com/PretendoNetwork/nex-go"
	"github.com/PretendoNetwork/nex-protocols-common-go/authentication"

	"github.com/Happynico7504/relaylink"
)

// PullFunc asks the main for a password the local store does not have.
type PullFunc func(game string, pid uint32) (string, error)

// PullTTL is how long a password fetched on demand stays in the local store.
const PullTTL = 10 * time.Minute

// Runner starts the auth servers of a configuration. The real one is Engine;
// tests substitute a fake so the process protocol can be tested without sockets.
type Runner interface {
	Start(games []relaylink.NexGame, store *Store, pull PullFunc) error
}

// Engine is the real Runner: one nex-go server per game, configured exactly like
// wsc-authentication / mk8-authentication / badge-arcade-authentication.
type Engine struct {
	Logf func(string, ...any)
}

func (e *Engine) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

// passwordFrom builds the PasswordFromPID function of one game: the local store
// first, then (once) the main.
func passwordFrom(game string, store *Store, pull PullFunc, logf func(string, ...any)) func(pid uint32) (string, uint32) {
	say := func(f string, a ...any) {
		if logf != nil {
			logf(f, a...)
		}
	}
	return func(pid uint32) (string, uint32) {
		if pw, ok := store.Get(game, pid); ok {
			return pw, 0
		}
		if pull == nil {
			say("nexauth: %s login for pid=%d: no credential in the store and no way to ask the main: InvalidUsername", game, pid)
			return "", nex.Errors.RendezVous.InvalidUsername
		}
		pw, err := pull(game, pid)
		if err == nil && pw != "" {
			say("nexauth: %s login for pid=%d: not in the store, fetched from the main", game, pid)
			store.Put(game, pid, pw, PullTTL)
			return pw, 0
		}
		// The console's login will be answered InvalidUsername, with no ticket: the console
		// then reports an error (106-0102) without ever asking for a secure-server ticket.
		say("nexauth: %s login for pid=%d: no credential (store empty, main said: %v, store holds %d entries): InvalidUsername", game, pid, err, store.Len())
		return "", nex.Errors.RendezVous.InvalidUsername
	}
}

// Validate rejects a configuration that would start a broken server.
func Validate(g relaylink.NexGame) error {
	switch {
	case g.Name == "" || g.Port < 1 || g.Port > 65535:
		return errors.New("game needs a name and a port")
	case g.AccessKey == "" || g.KerberosPassword == "" || g.SecureHost == "" || g.SecurePort == "":
		return fmt.Errorf("game %s: access key, Kerberos password and secure server address are all required", g.Name)
	}
	return nil
}

// Start launches every game's server. nex-go's Listen blocks and panics on
// failure (for example a port that is already in use), which is why this only
// ever runs in the child process; the panic ends the child and the parent
// restarts it.
func (e *Engine) Start(games []relaylink.NexGame, store *Store, pull PullFunc) error {
	for _, g := range games {
		if err := Validate(g); err != nil {
			return err
		}
	}
	for _, g := range games {
		g := g
		srv := nex.NewServer()
		srv.SetPRUDPVersion(1)
		srv.SetPRUDPProtocolMinorVersion(3)
		srv.SetDefaultNEXVersion(&nex.NEXVersion{Major: g.NEXMajor, Minor: g.NEXMinor, Patch: g.NEXPatch})
		srv.SetKerberosPassword(g.KerberosPassword)
		srv.SetAccessKey(g.AccessKey)

		auth := authentication.NewCommonAuthenticationProtocol(srv)
		secure := nex.NewStationURL("")
		secure.SetScheme("prudps")
		secure.SetAddress(g.SecureHost)
		secure.SetPort(g.SecurePort)
		secure.SetCID("1")
		secure.SetPID("2")
		secure.SetSID("1")
		secure.SetStream("10")
		secure.SetType("2")
		auth.SetSecureStationURL(secure)
		auth.SetBuildName(g.BuildName)
		auth.SetPasswordFromPIDFunction(passwordFrom(g.Name, store, pull, e.Logf))

		e.logf("nexauth: %s listening on :%d (secure server %s:%s)", g.Name, g.Port, g.SecureHost, g.SecurePort)
		go srv.Listen(fmt.Sprintf(":%d", g.Port))
	}
	return nil
}
