module github.com/Happynico7504/wscedge

go 1.19

require github.com/PretendoNetwork/nex-go v1.0.16

require (
	github.com/PretendoNetwork/plogger-go v1.0.2 // indirect
	github.com/fatih/color v1.13.0 // indirect
	github.com/jwalton/go-supportscolor v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.16 // indirect
	github.com/superwhiskers/crunch/v3 v3.5.6 // indirect
	golang.org/x/sys v0.0.0-20220825204002-c680a09ffe64 // indirect
	golang.org/x/term v0.0.0-20220722155259-a9ba230a4035 // indirect
)

// The same patched nex-go wsc-secure vendors (resend tracking, signature-before-decipher,
// counters). Stock v1.0.16 lacks reliable retransmission, so the edge must not use it.
replace github.com/PretendoNetwork/nex-go => ./nexgo
