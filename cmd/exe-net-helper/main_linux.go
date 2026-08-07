//go:build linux

package main

import (
	"flag"
	"fmt"
	"os"

	"exe/internal/fcnet"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "exe-net-helper:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: exe-net-helper <setup|cleanup> [options]")
	}
	action := os.Args[1]
	flags := flag.NewFlagSet("exe-net-helper "+action, flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	name := flags.String("name", "", "VM name")
	hostCIDR := flags.String("host-cidr", "", "host TAP IPv4 CIDR")
	outbound := flags.String("outbound", "", "outbound host interface")
	ownerUID := flags.Uint("owner-uid", 0, "unprivileged TAP owner uid")
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %v", flags.Args())
	}
	config := fcnet.Config{
		Name:     *name,
		HostCIDR: *hostCIDR,
		Outbound: *outbound,
		OwnerUID: uint32(*ownerUID),
	}
	switch action {
	case "setup":
		return fcnet.Setup(config)
	case "cleanup":
		return fcnet.Cleanup(config)
	default:
		return fmt.Errorf("unknown action %q", action)
	}
}
