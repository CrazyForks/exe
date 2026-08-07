//go:build linux

package fcnet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateConfinesNetwork(t *testing.T) {
	valid := Config{
		Name:     "test-vm",
		HostCIDR: "172.30.0.1/30",
		Outbound: "eth0",
		OwnerUID: uint32(os.Getuid()),
	}
	if valid.OwnerUID == 0 {
		valid.OwnerUID = 1000
	}
	if err := Validate(valid, false); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"name", func(c *Config) { c.Name = "../bad" }, "VM name"},
		{"root owner", func(c *Config) { c.OwnerUID = 0 }, "non-root"},
		{"public subnet", func(c *Config) { c.HostCIDR = "8.8.8.9/30" }, "private"},
		{"large subnet", func(c *Config) { c.HostCIDR = "172.30.0.1/24" }, "/30"},
		{"wrong host", func(c *Config) { c.HostCIDR = "172.30.0.2/30" }, "first usable"},
		{"helper outbound", func(c *Config) { c.Outbound = "exe-deadbeef00" }, "outbound"},
		{"invalid outbound", func(c *Config) { c.Outbound = "eth0;reboot" }, "outbound"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := valid
			test.mutate(&config)
			err := Validate(config, false)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestTapName(t *testing.T) {
	got := TapName("test-vm")
	if got != TapName("test-vm") {
		t.Fatal("TAP name is not deterministic")
	}
	if len(got) > 15 || !strings.HasPrefix(got, "exe-") {
		t.Fatalf("invalid TAP name %q", got)
	}
}

func TestCommandIgnoresCallerPathAndEnvironment(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ip")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 99\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("XTABLES_LIBDIR", dir)

	cmd, err := command("ip", "-Version")
	if err != nil {
		t.Fatal(err)
	}
	if cmd.Path == fake || !filepath.IsAbs(cmd.Path) {
		t.Fatalf("command resolved untrusted path %q", cmd.Path)
	}
	env := strings.Join(cmd.Env, "\n")
	if strings.Contains(env, dir) || strings.Contains(env, "XTABLES_LIBDIR") {
		t.Fatalf("command inherited untrusted environment:\n%s", env)
	}
	if !strings.Contains(env, "PATH="+trustedToolSearchPath) {
		t.Fatalf("command environment lacks trusted PATH:\n%s", env)
	}
	if _, err := command("sh", "-c", "true"); err == nil {
		t.Fatal("unexpectedly allowed a command outside the privileged allowlist")
	}
}
