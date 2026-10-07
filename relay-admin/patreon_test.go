package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestPatreonSignature(t *testing.T) {
	body := []byte(`{"data":{}}`)
	mac := hmac.New(md5.New, []byte("s3cret"))
	mac.Write(body)
	good := hex.EncodeToString(mac.Sum(nil))
	if !validPatreonSignature("s3cret", body, good) || !validPatreonSignature("s3cret", body, strings.ToUpper(good)) {
		t.Fatal("valid signature rejected")
	}
	if validPatreonSignature("s3cret", body, "deadbeef") || validPatreonSignature("other", body, good) || validPatreonSignature("s3cret", []byte("x"), good) {
		t.Fatal("invalid signature accepted")
	}
}

func TestParsePatreonMember(t *testing.T) {
	m, ok := parsePatreonMember([]byte(`{"data":{"id":"mem-1","type":"member","attributes":{"patron_status":"active_patron","last_charge_status":"Paid","last_charge_date":"2026-09-23T10:00:00.000+00:00","currently_entitled_amount_cents":500},"relationships":{"user":{"data":{"id":"u-9","type":"user"}}}}}`))
	if !ok || m.MemberID != "mem-1" || m.UserID != "u-9" || m.Cents != 500 || m.LastChargeStatus != "Paid" || m.PatronStatus != "active_patron" {
		t.Fatalf("bad parse: %+v ok=%v", m, ok)
	}
	if _, ok := parsePatreonMember([]byte(`{"data":{"type":"campaign","id":"1"}}`)); ok {
		t.Fatal("non-member payload accepted")
	}
	if _, ok := parsePatreonMember([]byte(`nope`)); ok {
		t.Fatal("garbage accepted")
	}
	// a null last_charge_date (never charged) must parse as empty, not fail
	m, ok = parsePatreonMember([]byte(`{"data":{"id":"m","type":"member","attributes":{"last_charge_date":null},"relationships":{"user":{"data":{"id":"u"}}}}}`))
	if !ok || m.LastChargeDate != "" {
		t.Fatalf("null date: %+v ok=%v", m, ok)
	}
}

func TestCoinsForCents(t *testing.T) {
	for _, c := range []struct {
		cents int
		per   float64
		want  int
	}{{500, 100, 500}, {300, 50, 150}, {99, 100, 99}, {0, 100, 0}, {500, 0, 0}, {-5, 100, 0},
		{300, 5, 15}, {300, 66.6667, 200}, {500, 66.6667, 333}, {100, 66.6667, 67}} {
		if got := coinsForCents(c.cents, c.per); got != c.want {
			t.Errorf("coinsForCents(%d,%v)=%d want %d", c.cents, c.per, got, c.want)
		}
	}
}

func TestPatreonTemplateRenders(t *testing.T) {
	var buf bytes.Buffer
	err := myPatreonTmpl.Execute(&buf, myPatreonData{PNID: "Alice", Configured: true, Crediting: true, Linked: true, PatreonName: "<b>Al</b>", HasWallet: true, Balance: 7,
		History: []coinLedgerRow{{Delta: 5, BalanceAfter: 7, Reason: "patreon", CreatedAt: time.Now()}}})
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Alice", "&lt;b&gt;Al&lt;/b&gt;", "Unlink", "adds coins to your wallet automatically"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	buf.Reset()
	myPatreonTmpl.Execute(&buf, myPatreonData{PNID: "Bob"})
	if !strings.Contains(buf.String(), "isn't set up yet") {
		t.Error("unconfigured state not shown")
	}
}
