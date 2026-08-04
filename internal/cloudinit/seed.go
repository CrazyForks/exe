// Package cloudinit builds NoCloud seed images for first-boot provisioning.
package cloudinit

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"

	"github.com/kdomanski/iso9660"
)

const userDataTmpl = `#cloud-config
hostname: %[1]s
manage_etc_hosts: true
users:
  - name: %[2]s
    sudo: ALL=(ALL) NOPASSWD:ALL
    groups: users, sudo
    shell: /bin/bash
    lock_passwd: true
    ssh_authorized_keys:
      - %[3]s
ssh_pwauth: false
disable_root: true
package_update: false
growpart:
  mode: auto
  devices: ["/"]
`

// BuildSeed writes a cloud-init NoCloud seed ISO (volume label "cidata")
// that creates a sudo-capable user with the given SSH key.
func BuildSeed(path, hostname, user, authorizedKey string) error {
	w, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer w.Cleanup()

	nonce := make([]byte, 4)
	rand.Read(nonce)
	userData := fmt.Sprintf(userDataTmpl, hostname, user, authorizedKey)
	metaData := fmt.Sprintf("instance-id: iid-%s-%s\nlocal-hostname: %s\n",
		hostname, hex.EncodeToString(nonce), hostname)

	if err := w.AddFile(strings.NewReader(userData), "user-data"); err != nil {
		return err
	}
	if err := w.AddFile(strings.NewReader(metaData), "meta-data"); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return w.WriteTo(f, "cidata")
}
