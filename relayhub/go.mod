module github.com/Happynico7504/relayhub

go 1.24

require github.com/Happynico7504/relaylink v0.0.0

require (
	github.com/PretendoNetwork/nex-go v1.0.16 // indirect
	github.com/PretendoNetwork/nex-protocols-common-go v1.0.17 // indirect
	github.com/PretendoNetwork/nex-protocols-go v1.0.23 // indirect
	github.com/PretendoNetwork/plogger-go v1.0.2 // indirect
	github.com/fatih/color v1.15.0 // indirect
	github.com/jwalton/go-supportscolor v1.1.0 // indirect
	github.com/mattn/go-colorable v0.1.13 // indirect
	github.com/mattn/go-isatty v0.0.18 // indirect
	github.com/oschwald/maxminddb-golang v1.13.1
	github.com/superwhiskers/crunch/v3 v3.5.7 // indirect
	golang.org/x/term v0.7.0 // indirect
)

require (
	github.com/Happynico7504/relayd v0.0.0
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/joho/godotenv v1.5.1
	github.com/lib/pq v1.12.3
	github.com/redis/go-redis/v9 v9.22.0
	go.uber.org/atomic v1.11.0 // indirect
	golang.org/x/sys v0.30.0 // indirect
)

replace github.com/Happynico7504/relaylink => ../relaylink

replace github.com/Happynico7504/relayd => ../relayd
