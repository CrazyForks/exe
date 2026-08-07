//go:build !darwin

package main

import (
	"context"
	"log"
	"strings"
	"time"

	"exe/internal/server"
)

// serveWait blocks until the daemon stops. Linux VMs are child processes, so
// shut them down before returning and triggering Firecracker's parent-death
// fallback.
func serveWait(srv *server.Server, wait func() error, shutdown func()) error {
	err := wait()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if running := srv.RunningVMNames(ctx); len(running) > 0 {
		log.Printf("daemon shutdown: stopping %s", strings.Join(running, ", "))
		srv.StopVMs(ctx, running)
	}
	shutdown()
	return err
}
