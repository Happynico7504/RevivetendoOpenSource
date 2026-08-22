package main

import (
	"os"
	"regexp"
	"testing"
)

// TestFixPolicylistUpdateTimeRealSample uses a real captured (broken)
// Pretendo policylist response - brotli-compressed, <UpdateTime> seconds
// field out of range (:85, not 0-59) - and confirms fixPolicylistUpdateTime
// detects and repairs it.
func TestFixPolicylistUpdateTimeRealSample(t *testing.T) {
	body, err := os.ReadFile("assets/policylist_broken_sample.br")
	if err != nil {
		t.Fatalf("read sample: %v", err)
	}

	fixed, changed, err := fixPolicylistUpdateTime(body, "br")
	if err != nil {
		t.Fatalf("fixPolicylistUpdateTime: %v", err)
	}
	if !changed {
		t.Fatal("expected the real sample's malformed UpdateTime to be detected")
	}

	validTime := regexp.MustCompile(`<UpdateTime>\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:[0-5]\d\+0000</UpdateTime>`)
	if !validTime.Match(fixed) {
		t.Fatalf("fixed body doesn't contain a validly-formatted UpdateTime: %s", fixed)
	}

	// The rest of the document (Priority entries etc.) must survive untouched.
	if !regexp.MustCompile(`<ListId>1924</ListId>`).Match(fixed) {
		t.Fatal("fix should only touch UpdateTime, not the rest of the document")
	}
}

func TestFixPolicylistUpdateTimeNoOp(t *testing.T) {
	_, changed, err := fixPolicylistUpdateTime([]byte("<PolicyList></PolicyList>"), "")
	if err != nil {
		t.Fatalf("fixPolicylistUpdateTime: %v", err)
	}
	if changed {
		t.Fatal("expected no change when there's no UpdateTime tag present")
	}
}
