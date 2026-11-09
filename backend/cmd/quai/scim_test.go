package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"testing"
)

func TestSCIMToken(t *testing.T) {
	run := func() (string, string) {
		t.Helper()
		cmd := newRootCmd()
		var out, errOut bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&errOut)
		cmd.SetArgs([]string{"scim-token"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		m := regexp.MustCompile(`^token: (quai_scim_[0-9a-f]{64})\nsha256: ([0-9a-f]{64})\n$`).FindStringSubmatch(out.String())
		if m == nil {
			t.Fatalf("output = %q", out.String())
		}
		if !bytes.Contains(errOut.Bytes(), []byte("not shown again")) {
			t.Errorf("no warning: %q", errOut.String())
		}
		return m[1], m[2]
	}
	token, sum := run()
	if h := sha256.Sum256([]byte(token)); hex.EncodeToString(h[:]) != sum {
		t.Errorf("sha256 = %s, not the token's", sum)
	}
	if again, _ := run(); again == token {
		t.Error("two tokens are the same")
	}
}
