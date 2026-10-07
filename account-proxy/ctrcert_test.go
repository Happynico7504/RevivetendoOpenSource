package main

import (
	"encoding/base64"
	"testing"
)

func TestIsCTRDeviceCert(t *testing.T) {
	mk := func(issuer string) string {
		b := make([]byte, 0x200)
		copy(b[0x84:], issuer)
		return base64.StdEncoding.EncodeToString(b)
	}
	if !isCTRDeviceCert(mk("Nintendo CA - G3_NintendoCTR2prod")) {
		t.Error("3DS issuer not recognised")
	}
	if isCTRDeviceCert(mk("Root-CA00000003-MS00000012")) {
		t.Error("Wii U issuer misclassified as 3DS")
	}
	for _, bad := range []string{"", "not base64!!", "AAAA"} {
		if isCTRDeviceCert(bad) {
			t.Errorf("%q classified as 3DS", bad)
		}
	}
	// header values are sometimes unpadded
	c := mk("Nintendo CA - G3_NintendoCTR2prod")
	for len(c) > 0 && c[len(c)-1] == '=' {
		c = c[:len(c)-1]
	}
	if !isCTRDeviceCert(c) {
		t.Error("unpadded 3DS cert not recognised")
	}
}
