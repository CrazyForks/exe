package cloudinit

import (
	"strings"
	"testing"
)

func TestDocumentsDiskModes(t *testing.T) {
	growing, _ := Documents("vm", "dev", "ssh-ed25519 test", true)
	for _, want := range []string{"name: dev", "ssh-ed25519 test", "mode: auto"} {
		if !strings.Contains(growing, want) {
			t.Fatalf("growing user-data does not contain %q:\n%s", want, growing)
		}
	}
	fixed, _ := Documents("vm", "dev", "ssh-ed25519 test", false)
	for _, want := range []string{"mode: 'off'", "resize_rootfs: false"} {
		if !strings.Contains(fixed, want) {
			t.Fatalf("fixed user-data does not contain %q:\n%s", want, fixed)
		}
	}
}
