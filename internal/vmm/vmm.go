// Package vmm manages virtual machines behind a platform-neutral interface.
// The only implementation today uses Apple's Virtualization.framework
// (darwin); a Linux backend (KVM via cloud-hypervisor/QEMU) can be added as
// another build-tagged file implementing Manager.
package vmm

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"
)

type Spec struct {
	Name     string `json:"name"`
	CPUs     int    `json:"cpus"`
	MemoryMB int    `json:"memory_mb"`
	DiskGB   int    `json:"disk_gb"`
}

type Info struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	CPUs      int       `json:"cpus"`
	MemoryMB  int       `json:"memory_mb"`
	DiskGB    int       `json:"disk_gb"`
	MAC       string    `json:"mac"`
	IP        string    `json:"ip,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Manager interface {
	// Create provisions a new VM from the base image and boots it.
	Create(ctx context.Context, spec Spec) (*Info, error)
	Start(ctx context.Context, name string) (*Info, error)
	Stop(ctx context.Context, name string) error
	Delete(ctx context.Context, name string) error
	List(ctx context.Context) ([]*Info, error)
	Get(ctx context.Context, name string) (*Info, error)
}

// ImageEnsurer is implemented by managers that can pre-fetch their base image.
type ImageEnsurer interface {
	EnsureImage(ctx context.Context) (string, error)
}

type Options struct {
	StateDir      string
	ImageURL      string
	SSHUser       string
	AuthorizedKey string
}

var (
	ErrNotFound   = errors.New("vm not found")
	ErrNotRunning = errors.New("vm is not running (VMs live inside the `exe serve` process)")
)

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)

func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid vm name %q (use lowercase letters, digits, hyphens)", name)
	}
	return nil
}
