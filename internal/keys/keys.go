// Package keys manages the service SSH keypair used to log in to VMs.
package keys

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Ensure generates the keypair on first use and returns the private key path
// and the authorized_keys line to inject into VMs via cloud-init.
func Ensure(stateDir string) (privPath, authorizedKey string, err error) {
	dir := filepath.Join(stateDir, "ssh")
	privPath = filepath.Join(dir, "id_ed25519")
	pubPath := privPath + ".pub"
	if b, rerr := os.ReadFile(pubPath); rerr == nil {
		if _, serr := os.Stat(privPath); serr == nil {
			return privPath, strings.TrimSpace(string(b)), nil
		}
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "exe")
	if err != nil {
		return "", "", err
	}
	if err = os.WriteFile(privPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", "", err
	}
	authorizedKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub))) + " exe"
	if err = os.WriteFile(pubPath, []byte(authorizedKey+"\n"), 0o644); err != nil {
		return "", "", err
	}
	return privPath, authorizedKey, nil
}
