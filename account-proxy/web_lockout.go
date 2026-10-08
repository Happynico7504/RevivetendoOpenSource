package main

// Web password lockout and messages, shared through Redis by account-proxy (Juxt web login,
// /internal/auth) and relay-admin (dashboard /my/login): 3 wrong web passwords
// for a PNID within 5 minutes lock that PNID's web login for 15 minutes, from
// anywhere. Keyed on the account, not the client IP: every web login reaches
// this server through the dashboard box's Apache, so there is no client
// address here to trust or to firewall. Keep both copies of this file the same.

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// webPasswordHelpURL explains how to set a web password (the website's own page).
const webPasswordHelpURL = "https://revivetendo.nicochristmann.net/web-password.html"

// webPasswordMissingMessage is shown when an account has no web password yet.
// Juxt (through grpc-stubs) matches its "No web password" prefix.
const webPasswordMissingMessage = "No web password is set for this account yet. See " + webPasswordHelpURL + " to set one."

// revivetendoGuideURL is the website's setup guide for connecting a console.
const revivetendoGuideURL = "https://revivetendo.nicochristmann.net/guide.html"

// pnidNotOnRevivetendoMessage: no Wii U or 3DS has ever logged in with the PNID.
// Juxt (through grpc-stubs) matches its "This PNID is not on Revivetendo" prefix.
const pnidNotOnRevivetendoMessage = "This PNID is not on Revivetendo."

// pnidCaseMismatchMessage: the PNID exists, but typed with different upper/lowercase
// letters. Juxt (through grpc-stubs) matches its "PNIDs are case-sensitive" prefix.
const pnidCaseMismatchMessage = "PNIDs are case-sensitive: this one exists with different upper/lowercase letters. Type it exactly as on your console."

const (
	webPasswordMaxFails = 3
	webPasswordWindow   = 5 * time.Minute
	webPasswordLockout  = 15 * time.Minute
)

func webLockoutKeys(pnid string) (fails, lock string) {
	p := strings.ToLower(strings.TrimSpace(pnid))
	return "webpw:fails:" + p, "webpw:lock:" + p
}

// webPasswordLocked is how much longer pnid's web login stays locked (0 = not locked).
func webPasswordLocked(pnid string) time.Duration {
	_, lock := webLockoutKeys(pnid)
	ttl, err := runtimeCacheClient.TTL(context.Background(), lock).Result()
	if err != nil || ttl <= 0 {
		return 0
	}
	return ttl
}

// webPasswordFailed records a wrong password and reports whether this one locked
// the account.
func webPasswordFailed(pnid string) bool {
	ctx := context.Background()
	fails, lock := webLockoutKeys(pnid)
	n, err := runtimeCacheClient.Incr(ctx, fails).Result()
	if err != nil {
		return false
	}
	if n == 1 {
		runtimeCacheClient.Expire(ctx, fails, webPasswordWindow)
	}
	if n < webPasswordMaxFails {
		return false
	}
	runtimeCacheClient.Set(ctx, lock, "1", webPasswordLockout)
	runtimeCacheClient.Del(ctx, fails)
	return true
}

// webPasswordSucceeded clears the wrong-password count after a correct login.
func webPasswordSucceeded(pnid string) {
	fails, _ := webLockoutKeys(pnid)
	runtimeCacheClient.Del(context.Background(), fails)
}

// webLockoutMessage tells the player how long to wait.
func webLockoutMessage(left time.Duration) string {
	m := int(left.Round(time.Minute) / time.Minute)
	if m < 1 {
		m = 1
	}
	wait := strconv.Itoa(m) + " minutes"
	if m == 1 {
		wait = "1 minute"
	}
	return "Too many wrong passwords. Try again in " + wait + ", or unlock it with /unlock_web_login on Discord."
}
