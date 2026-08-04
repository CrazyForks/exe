//go:build !darwin

package main

import "exe/internal/server"

// serveWait blocks until the daemon stops; only macOS adds a menu bar icon.
func serveWait(srv *server.Server, wait func() error, shutdown func()) error {
	return wait()
}
