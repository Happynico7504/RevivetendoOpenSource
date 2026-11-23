//go:build !linux

package mk8edge

import "time"

func icmpSummary(string, int, time.Duration, time.Duration) (string, bool) { return "", false }
