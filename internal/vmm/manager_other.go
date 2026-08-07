//go:build !darwin && !linux

package vmm

import "fmt"

// New returns an error on platforms without a VM backend.
func New(opts Options) (Manager, error) {
	return nil, fmt.Errorf("no vm backend for this platform")
}
