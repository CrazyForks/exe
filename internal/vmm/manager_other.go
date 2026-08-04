//go:build !darwin

package vmm

import "fmt"

// New returns an error on non-darwin platforms: only the macOS
// Virtualization.framework backend exists today. A Linux backend (KVM via
// cloud-hypervisor, firecracker, or QEMU) can implement Manager next to this
// file with the same Options.
func New(opts Options) (Manager, error) {
	return nil, fmt.Errorf("no vm backend for this platform yet (only darwin/Virtualization.framework is implemented)")
}
