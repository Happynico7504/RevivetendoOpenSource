package main

import "testing"

func TestCASServiceTitleFor(t *testing.T) {
	for app, want := range map[string]string{
		"0004000000153500": "0004000D00153500", // USA
		"0004000000153600": "0004000D00153600", // EUR/JPN
		"":                 "0004000D00153600",
	} {
		if got := casServiceTitleFor(app); got != want {
			t.Errorf("%q: got %s, want %s", app, got, want)
		}
	}
}
