//go:build darwin

package vmm

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// macOS's built-in DHCP server (bootpd) records leases for NAT VMs here.
const leasesPath = "/var/db/dhcpd_leases"

type leaseEntry struct {
	ip    string
	hw    string // client identifier payload after the type prefix
	name  string
	lease uint64 // expiry timestamp (hex in file); newer wins
}

func (e leaseEntry) fingerprint() string {
	return e.ip + "|" + e.hw + "|" + strconv.FormatUint(e.lease, 16)
}

func parseLeases() ([]leaseEntry, error) {
	b, err := os.ReadFile(leasesPath)
	if err != nil {
		return nil, err
	}
	var out []leaseEntry
	var cur leaseEntry
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "{":
			cur = leaseEntry{}
		case strings.HasPrefix(line, "ip_address="):
			cur.ip = strings.TrimPrefix(line, "ip_address=")
		case strings.HasPrefix(line, "name="):
			cur.name = strings.TrimPrefix(line, "name=")
		case strings.HasPrefix(line, "lease="):
			v := strings.TrimPrefix(strings.TrimPrefix(line, "lease="), "0x")
			cur.lease, _ = strconv.ParseUint(v, 16, 64)
		case strings.HasPrefix(line, "hw_address="):
			hw := strings.TrimPrefix(line, "hw_address=")
			if i := strings.IndexByte(hw, ','); i >= 0 {
				hw = hw[i+1:]
			}
			cur.hw = hw
		case line == "}":
			if cur.ip != "" {
				out = append(out, cur)
			}
		}
	}
	return out, nil
}

// leaseFingerprints snapshots the lease table before a boot so stale
// entries can be ignored while waiting for the fresh lease to appear.
func leaseFingerprints() map[string]bool {
	entries, err := parseLeases()
	if err != nil {
		return nil
	}
	fps := make(map[string]bool, len(entries))
	for _, e := range entries {
		fps[e.fingerprint()] = true
	}
	return fps
}

// normalizeMAC canonicalizes a MAC; dhcpd_leases strips leading zeros
// per octet (e.g. "aa:b:cc"), so both sides must be normalized.
func normalizeMAC(s string) string {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 6 {
		return ""
	}
	out := make([]string, len(parts))
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 16, 8)
		if err != nil {
			return ""
		}
		out[i] = fmt.Sprintf("%02x", v)
	}
	return strings.Join(out, ":")
}

// lookupLease finds a VM's IP. Clients that identify by MAC produce
// "hw_address=1,<mac>" entries and are matched directly — a MAC is unique to
// one VM, so those matches are always trusted. Debian 13's dhcpcd sends an
// RFC 4361 DUID instead ("hw_address=ff,<18 bytes>"), which hides the MAC;
// for those we fall back to matching the lease name (the hostname cloud-init
// set, which equals the VM name), skipping entries in exclude (the pre-boot
// snapshot) so a recreated VM never matches its predecessor's stale lease.
// Among matches, the newest lease timestamp wins.
func lookupLease(mac, vmName string, exclude map[string]bool) (string, error) {
	want := normalizeMAC(mac)
	if want == "" {
		return "", fmt.Errorf("bad mac %q", mac)
	}
	entries, err := parseLeases()
	if err != nil {
		return "", err
	}
	bestIP, bestLease := "", uint64(0)
	for _, e := range entries {
		match := normalizeMAC(e.hw) == want
		if !match && vmName != "" && e.name == vmName && !exclude[e.fingerprint()] {
			match = true
		}
		if match && e.lease >= bestLease {
			bestIP, bestLease = e.ip, e.lease
		}
	}
	if bestIP == "" {
		return "", fmt.Errorf("no DHCP lease for mac %s / name %s", mac, vmName)
	}
	return bestIP, nil
}

func waitIP(ctx context.Context, mac, vmName string, exclude map[string]bool, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		if ip, err := lookupLease(mac, vmName, exclude); err == nil {
			return ip, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func waitTCP(ctx context.Context, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		c, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err == nil {
			c.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
