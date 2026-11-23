// Command mk8edge runs the Mario Kart 8 PRUDP terminator standalone with the echo backend
// (milestone 1). The Kerberos password comes from MK8_KERBEROS_PASSWORD, like mk8-secure.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/Happynico7504/mk8edge"
)

// Version is stamped at build time (go build -ldflags "-X main.Version=N"). The relay's
// updater runs `mk8edge -version` as a pre-flight before installing a release, and
// requires exactly "mk8edge N".
var Version = "0"

func main() {
	port := flag.Int("port", 60103, "UDP port (a TEST port, not the production secure port)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	usePipe := flag.Bool("pipe", false, "run under relayd: talk to it over descriptors 3 and 4 (default: standalone echo)")
	flag.Parse()
	if *showVersion {
		fmt.Printf("mk8edge %s\n", Version)
		return
	}
	pw := os.Getenv("MK8_KERBEROS_PASSWORD")
	if pw == "" {
		log.Fatal("mk8edge: MK8_KERBEROS_PASSWORD is not set")
	}
	cfg := mk8edge.Config{Port: *port, KerberosPassword: pw, Logf: log.Printf}
	if *usePipe {
		// Started by relayd: it talks to us over inherited descriptors 3 (from relayd) and 4
		// (to relayd), and its end of the pipe closing means relayd is gone.
		b := &mk8edge.PipeBackend{Logf: log.Printf}
		e := mk8edge.New(cfg, b)
		b.Edge = e
		go func() {
			err := b.Run(os.NewFile(3, "from-relayd"), os.NewFile(4, "to-relayd"))
			log.Fatalf("mk8edge: pipe to relayd closed (%v): exiting", err)
		}()
		e.Serve()
		return
	}
	b := &mk8edge.EchoBackend{}
	e := mk8edge.New(cfg, b)
	b.Edge = e
	e.Serve()
}
