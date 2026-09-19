// Command wscedge runs the WSC PRUDP terminator standalone with the echo backend
// (milestone 1). The Kerberos password comes from WSC_KERBEROS_PASSWORD, like wsc-secure.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Happynico7504/wscedge"
)

// Version is stamped at build time (go build -ldflags "-X main.Version=N"). The relay's
// updater runs `wscedge -version` as a pre-flight before installing a release, and
// requires exactly "wscedge N".
var Version = "0"

func main() {
	port := flag.Int("port", 60115, "UDP port (a TEST port, not the production secure port)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Printf("wscedge %s\n", Version)
		return
	}
	pw := os.Getenv("WSC_KERBEROS_PASSWORD")
	if pw == "" {
		log.Fatal("wscedge: WSC_KERBEROS_PASSWORD is not set")
	}
	e := wscedge.New(wscedge.Config{Port: *port, KerberosPassword: pw, Logf: log.Printf}, wscedge.EchoBackend{})
	e.Serve()
}
