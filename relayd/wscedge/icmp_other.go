//go:build !linux

package wscedge

import "time"

func icmpSummary(string, int, time.Duration, time.Duration) (string, bool) { return "", false }
