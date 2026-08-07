//go:build linux

// Package fcnet implements the narrow privileged operations needed to connect
// Firecracker TAP devices to host NAT.
package fcnet

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

type Config struct {
	Name     string
	HostCIDR string
	Outbound string
	OwnerUID uint32
}

const trustedToolSearchPath = "/usr/sbin:/usr/bin:/sbin:/bin"

var (
	nameRE           = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,30}$`)
	interfaceRE      = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,15}$`)
	trustedToolDirs  = []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"}
	trustedToolNames = map[string]bool{
		"ip":       true,
		"iptables": true,
	}
)

func TapName(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("exe-%x", sum[:5])
}

// Validate confines the helper to deterministic exe TAP devices and private
// /30 networks whose host address is the first usable address.
func Validate(config Config, requireOutbound bool) error {
	if !nameRE.MatchString(config.Name) {
		return fmt.Errorf("invalid VM name %q", config.Name)
	}
	if config.OwnerUID == 0 {
		return errors.New("TAP owner uid must be non-root")
	}
	ip, network, err := net.ParseCIDR(config.HostCIDR)
	if err != nil || ip.To4() == nil {
		return fmt.Errorf("invalid host CIDR %q", config.HostCIDR)
	}
	ones, bits := network.Mask.Size()
	if bits != 32 || ones != 30 {
		return errors.New("host CIDR must be an IPv4 /30")
	}
	ip = ip.To4()
	network.IP = network.IP.To4()
	if !ip.IsPrivate() || !network.IP.IsPrivate() {
		return errors.New("host CIDR must use a private IPv4 range")
	}
	if ipv4Number(ip) != ipv4Number(network.IP)+1 {
		return errors.New("host address must be the first usable address in its /30")
	}
	if !interfaceRE.MatchString(config.Outbound) || strings.HasPrefix(config.Outbound, "exe-") {
		return fmt.Errorf("invalid outbound interface %q", config.Outbound)
	}
	if requireOutbound {
		if _, err := net.InterfaceByName(config.Outbound); err != nil {
			return fmt.Errorf("find outbound interface %q: %w", config.Outbound, err)
		}
	}
	return nil
}

func Setup(config Config) error {
	if err := validatePrivilege(config); err != nil {
		return err
	}
	if err := Validate(config, true); err != nil {
		return err
	}
	if err := cleanup(config); err != nil {
		return fmt.Errorf("clean existing network: %w", err)
	}

	if data, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward"); err != nil {
		return fmt.Errorf("read IPv4 forwarding setting: %w", err)
	} else if strings.TrimSpace(string(data)) != "1" {
		if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", []byte("1\n"), 0o644); err != nil {
			return fmt.Errorf("enable IPv4 forwarding: %w", err)
		}
	}

	tap := TapName(config.Name)
	rollback := true
	defer func() {
		if rollback {
			_ = cleanup(config)
		}
	}()
	if err := run("ip", "tuntap", "add", "dev", tap, "mode", "tap", "user", strconv.FormatUint(uint64(config.OwnerUID), 10)); err != nil {
		return err
	}
	if err := run("ip", "addr", "add", config.HostCIDR, "dev", tap); err != nil {
		return err
	}
	if err := run("ip", "link", "set", "dev", tap, "up"); err != nil {
		return err
	}
	for _, rule := range rules(config) {
		if err := ensureRule(rule); err != nil {
			return err
		}
	}
	rollback = false
	return nil
}

func Cleanup(config Config) error {
	if err := validatePrivilege(config); err != nil {
		return err
	}
	if err := Validate(config, false); err != nil {
		return err
	}
	return cleanup(config)
}

func validatePrivilege(config Config) error {
	if uint32(os.Getuid()) != config.OwnerUID {
		return errors.New("TAP owner uid must match the helper caller")
	}
	return nil
}

type firewallRule struct {
	table  string
	chain  string
	args   []string
	insert bool
}

func rules(config Config) []firewallRule {
	_, source, _ := net.ParseCIDR(config.HostCIDR)
	tap := TapName(config.Name)
	comment := "exe:" + config.Name
	return []firewallRule{
		{
			table: "nat",
			chain: "POSTROUTING",
			args:  []string{"-s", source.String(), "-o", config.Outbound, "-m", "comment", "--comment", comment, "-j", "MASQUERADE"},
		},
		{
			chain:  "FORWARD",
			insert: true,
			args:   []string{"-i", tap, "-o", config.Outbound, "-m", "comment", "--comment", comment, "-j", "ACCEPT"},
		},
		{
			chain:  "FORWARD",
			insert: true,
			args:   []string{"-i", config.Outbound, "-o", tap, "-m", "conntrack", "--ctstate", "RELATED,ESTABLISHED", "-m", "comment", "--comment", comment, "-j", "ACCEPT"},
		},
	}
}

func cleanup(config Config) error {
	var errs []error
	list := rules(config)
	for i := len(list) - 1; i >= 0; i-- {
		if err := deleteRule(list[i]); err != nil {
			errs = append(errs, err)
		}
	}
	tap := TapName(config.Name)
	if _, err := net.InterfaceByName(tap); err == nil {
		if err := run("ip", "link", "delete", "dev", tap); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func ensureRule(rule firewallRule) error {
	exists, err := ruleExists(rule)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	action := "-A"
	if rule.insert {
		action = "-I"
	}
	args := ruleArgs(rule, action)
	if rule.insert {
		chainEnd := 3
		if rule.table != "" {
			chainEnd = 5
		}
		args = append(args[:chainEnd], append([]string{"1"}, args[chainEnd:]...)...)
	}
	return run("iptables", args...)
}

func deleteRule(rule firewallRule) error {
	for {
		exists, err := ruleExists(rule)
		if err != nil {
			return err
		}
		if !exists {
			return nil
		}
		if err := run("iptables", ruleArgs(rule, "-D")...); err != nil {
			return err
		}
	}
}

func ruleArgs(rule firewallRule, action string) []string {
	args := []string{"-w"}
	if rule.table != "" {
		args = append(args, "-t", rule.table)
	}
	args = append(args, action, rule.chain)
	return append(args, rule.args...)
}

func ruleExists(rule firewallRule) (bool, error) {
	args := ruleArgs(rule, "-C")
	cmd, err := command("iptables", args...)
	if err != nil {
		return false, err
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	text := strings.TrimSpace(string(output))
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 &&
		(text == "" || strings.Contains(text, "Bad rule")) {
		return false, nil
	}
	return false, commandError("iptables", args, output, err)
}

func run(name string, args ...string) error {
	cmd, err := command(name, args...)
	if err != nil {
		return err
	}
	output, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	return commandError(name, args, output, err)
}

func commandError(name string, args []string, output []byte, err error) error {
	text := strings.TrimSpace(string(output))
	if text == "" {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
}

func command(name string, args ...string) (*exec.Cmd, error) {
	path, err := trustedTool(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(path, args...)
	cmd.Env = []string{
		"LANG=C",
		"LC_ALL=C",
		"PATH=" + trustedToolSearchPath,
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{AmbientCaps: []uintptr{unix.CAP_NET_ADMIN}}
	return cmd, nil
}

func trustedTool(name string) (string, error) {
	if !trustedToolNames[name] {
		return "", fmt.Errorf("privileged command %q is not allowed", name)
	}
	for _, dir := range trustedToolDirs {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect privileged command %s: %w", path, err)
		}
		dirInfo, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("inspect privileged command directory %s: %w", dir, err)
		}
		if err := validateTrustedPath(dir, dirInfo, true); err != nil {
			return "", err
		}
		if err := validateTrustedPath(path, info, false); err != nil {
			return "", err
		}
		if size, err := unix.Getxattr(path, "security.capability", nil); err == nil && size > 0 {
			return "", fmt.Errorf("privileged command %s must not have file capabilities", path)
		} else if err != nil && !errors.Is(err, unix.ENODATA) && !errors.Is(err, unix.ENOTSUP) {
			return "", fmt.Errorf("inspect capabilities on privileged command %s: %w", path, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("trusted privileged command %q not found in %s", name, trustedToolSearchPath)
}

func validateTrustedPath(path string, info os.FileInfo, directory bool) error {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || sys.Uid != 0 {
		return fmt.Errorf("privileged command path %s must be owned by root", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("privileged command path %s must not be group- or world-writable", path)
	}
	if directory {
		if !info.IsDir() {
			return fmt.Errorf("privileged command path %s must be a directory", path)
		}
		return nil
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("privileged command %s must be a regular executable", path)
	}
	if info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return fmt.Errorf("privileged command %s must not be setuid or setgid", path)
	}
	return nil
}

func ipv4Number(ip net.IP) uint32 {
	return binary.BigEndian.Uint32(ip.To4())
}
