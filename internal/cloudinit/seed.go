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
%[4]s`

// BuildSeed writes a cloud-init NoCloud seed ISO (volume label "cidata")
// that creates a sudo-capable user with the given SSH key.
func BuildSeed(path, hostname, user, authorizedKey string) error {
	return BuildSeedWithNetwork(path, hostname, user, authorizedKey, "")
}

// BuildSeedWithNetwork optionally adds a NoCloud network-config document.
func BuildSeedWithNetwork(path, hostname, user, authorizedKey, networkConfig string) error {
	w, err := iso9660.NewWriter()
	if err != nil {
		return err
	}
	defer w.Cleanup()

	userData, metaData := Documents(hostname, user, authorizedKey, true)
	if err := w.AddFile(strings.NewReader(userData), "user-data"); err != nil {
		return err
	}
	if err := w.AddFile(strings.NewReader(metaData), "meta-data"); err != nil {
		return err
	}
	if networkConfig != "" {
		if err := w.AddFile(strings.NewReader(networkConfig), "network-config"); err != nil {
			return err
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return w.WriteTo(f, "cidata")
}

// Documents returns the NoCloud user-data and meta-data documents. growDisk is
// true for whole-disk EFI images and false for Firecracker rootfs images that
// the host has already resized.
func Documents(hostname, user, authorizedKey string, growDisk bool) (string, string) {
	nonce := make([]byte, 4)
	_, _ = rand.Read(nonce)
	diskConfig := `growpart:
  mode: auto
  devices: ["/"]
`
	if !growDisk {
		diskConfig = "growpart:\n  mode: 'off'\nresize_rootfs: false\n"
	}
	userData := fmt.Sprintf(userDataTmpl, hostname, user, authorizedKey, diskConfig)
	metaData := fmt.Sprintf("instance-id: iid-%s-%s\nlocal-hostname: %s\n",
		hostname, hex.EncodeToString(nonce), hostname)
	return userData, metaData
}
