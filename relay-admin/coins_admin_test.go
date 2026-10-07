package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestCoinsTemplateRenders(t *testing.T) {
	data := struct {
		Wallets      []coinWalletRow
		Ledger       []coinLedgerRow
		Msg, Q       string
		Nonce        string
		Total, Coins int
	}{
		Wallets: []coinWalletRow{{PID: 1, PNID: "Alice", Balance: 7, LastGrant: time.Now()}, {PID: 2, Balance: 0, LastGrant: time.Now()}},
		Ledger:  []coinLedgerRow{{PID: 1, PNID: "Alice", Delta: 5, BalanceAfter: 7, Reason: "admin: <b>x</b>", CreatedAt: time.Now()}, {PID: 2, Delta: -1, BalanceAfter: 0, Reason: "purchase", CreatedAt: time.Now()}},
		Msg:     "ok", Q: "al", Nonce: "abc", Total: 2, Coins: 7,
	}
	var buf bytes.Buffer
	if err := coinsTmpl.Execute(&buf, data); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"Alice", "&lt;b&gt;x&lt;/b&gt;", `name="nonce" value="abc"`, "purchase"} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page missing %q", want)
		}
	}
}
