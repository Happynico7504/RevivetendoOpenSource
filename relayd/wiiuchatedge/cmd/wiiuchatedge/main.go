// Command wiiuchatedge runs the Wii U Chat PRUDP terminator standalone with the echo backend
// (milestone 1). The Kerberos password comes from PN_WUC_KERBEROS_PASSWORD, like wiiu-chat.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Happynico7504/wiiuchatedge"
)

// Version is stamped at build time (go build -ldflags "-X main.Version=N"). The relay's updater
// runs `wiiuchatedge -version` as a pre-flight and requires exactly "wiiuchatedge N".
var Version = "0"

func main() {
	port := flag.Int("port", 60125, "UDP port (a TEST port, not the production secure port 60005)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("wiiuchatedge %s\n", Version)
		return
	}
	pw := os.Getenv("PN_WUC_KERBEROS_PASSWORD")
	if pw == "" {
		log.Fatal("wiiuchatedge: PN_WUC_KERBEROS_PASSWORD is not set")
	}
	b := &wiiuchatedge.EchoBackend{}
	e := wiiuchatedge.New(wiiuchatedge.Config{Port: *port, KerberosPassword: pw, Logf: log.Printf}, b)
	b.Edge = e
	e.Serve()
}
