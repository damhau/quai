package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/damhau/quai/backend/internal/config"
	"github.com/damhau/quai/backend/internal/repositories"
	"github.com/damhau/quai/backend/internal/secrets"
)

// TestSignedCommits: with git.signingKey, Quai's commits are signed with that SSH key, which git
// verify-commit checks against the allowed signers; in direct mode the commit, made after the
// approvals, carries them as Approved-by trailers.
func TestSignedCommits(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("no ssh-keygen")
	}
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	key := string(pem.EncodeToMemory(block))
	dir := t.TempDir()
	bare, _ := localRemote(t, dir)
	vars := map[string]string{"QUAI_SIGNING_KEY": key}
	h := start(t, dir, `connector: generic
  url: `+bare+`
  sync: { minInterval: 1ns }`, repositories.Options{Secrets: secrets.New(map[string]secrets.Backend{"env": secrets.Env{Lookup: func(n string) (string, bool) { v, ok := vars[n]; return v, ok }}})},
		func(c *config.Config) { c.Git.SigningKey.Ref = "env:QUAI_SIGNING_KEY" })
	h.bare = bare
	bob, alice := h.as("bob"), h.as("alice")
	var r RequestView
	if status, body := bob.do(http.MethodPost, Prefix+"/requests/type", resizeBody(6), &r); status != http.StatusCreated {
		t.Fatalf("submit = %d %s", status, body)
	}
	alice.do(http.MethodPost, Prefix+"/requests/"+r.ID+"/review", map[string]string{"decision": "approve"}, nil)
	bob.await(r.ID, "applied")

	sshPub, _ := ssh.NewPublicKey(pub)
	signers := filepath.Join(t.TempDir(), "allowed_signers")
	if err := os.WriteFile(signers, []byte("quai@localhost "+string(ssh.MarshalAuthorizedKey(sshPub))), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "--git-dir", bare, "-c", "gpg.ssh.allowedSignersFile="+signers, "verify-commit", "main") // #nosec G204 -- test
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "TMPDIR=" + os.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("verify-commit: %v: %s", err, out)
	}
	msg := git(t, "", "--git-dir", bare, "log", "-1", "--format=%B", "main")
	if !strings.Contains(msg, "Approved-by: alice") || !strings.Contains(msg, "Requested-by: bob") {
		t.Errorf("the commit's message:\n%s", msg)
	}
}
