//go:build !linux

package wiiuchatedge

import "time"

func icmpSummary(string, int, time.Duration, time.Duration) (string, bool) { return "", false }
