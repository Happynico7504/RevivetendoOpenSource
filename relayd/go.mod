module github.com/Happynico7504/relayd

go 1.23

require (
	github.com/Happynico7504/relaylink v0.0.0
	github.com/PretendoNetwork/nex-go v1.0.16
	github.com/PretendoNetwork/nex-protocols-common-go v1.0.17
)

require (
	github.com/PretendoNetwork/nex-protocols-go v1.0.23 // indirect
	github.com/PretendoNetwork/plogger-go v1.0.2 // indirect
	github.com/fatih/color v1.15.0 // indirect
	github.com/jwalton/go-supportscolor v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.18 // indirect
	github.com/superwhiskers/crunch/v3 v3.5.7 // indirect
	golang.org/x/sys v0.7.0 // indirect
	golang.org/x/term v0.7.0 // indirect
)

replace github.com/Happynico7504/relaylink => ../relaylink

// Patched copy: tickets carry the real issue time (see third_party/nex-protocols-common-go/PATCHED.md).
replace github.com/PretendoNetwork/nex-protocols-common-go => ./third_party/nex-protocols-common-go
