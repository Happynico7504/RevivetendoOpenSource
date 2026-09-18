package globals

import "net/url"

// RelayThroughHPPHost rewrites a real Exoscale presigned URL to instead route
// through account-proxy's already-3DS-trusted hpp-001a2c00-l1.n.app.nicoch.net
// host (Swapdoodle's own HPP hostname - trust here is per-hostname, not
// per-title, so any title can reuse it), under /s3relay/<real-host>/<real-path>.
//
// The 3DS trusts zero root CAs per-title until that title's own compiled code
// explicitly adds one (see 3dbrew's SSL Services page); Badge Arcade's retail
// code only ever added trust for Nintendo's own infrastructure, so a direct
// HTTPS POST from the console to Exoscale's real GandiCert/DigiCert-chained
// cert silently never lands even though the RMC-level upload sequence
// completes "successfully" - confirmed 2026-09-15 by finding zero objects
// under this prefix in the bucket despite a completed CompletePostObject
// call. This is the exact same issue already solved for Swapdoodle (see
// swapdoodle/globals/s3_presigner.go's relayThroughHPPHost and account-
// proxy's handleSwapdoodleS3Relay) - reusing that same proven relay here
// instead of re-solving it, since the relay itself only inspects the
// bucket/key form fields, not which game they belong to.
func RelayThroughHPPHost(rawURL string) string {
	real, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	// account-proxy's handleSwapdoodleS3Relay requires a "/" after <real-host>
	// to find where the real path starts (rest[:slash]/rest[slash:]) - it 400s
	// with "bad s3relay path" before ever checking for a multipart POST if
	// that slash is missing. Swap Doodle's own minio-go presigner always
	// returns a path-style URL (host+"/"+bucket), so this never came up there,
	// but Badge Arcade's hand-rolled PresignPostObject returns a bare
	// virtual-hosted-style URL with an empty path - confirmed 2026-09-16 this
	// silently 400s every upload attempt (unlogged, since that branch has no
	// log line). Default to "/" so the path is never empty.
	realPath := real.Path
	if realPath == "" {
		realPath = "/"
	}
	relay := &url.URL{
		Scheme:   "https",
		Host:     "hpp-001a2c00-l1.n.app.nicoch.net",
		Path:     "/s3relay/" + real.Host + realPath,
		RawQuery: real.RawQuery,
	}
	return relay.String()
}
