//go:build darwin

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"exe/internal/menubar"
	"exe/internal/server"
)

// serveWait parks cmdServe for the life of the daemon. On macOS the main
// thread runs the AppKit loop so a menu bar icon offers Open Web UI /
// Restart Daemon / Quit; the signal-and-error wait moves to a goroutine
// that exits the process, because the AppKit loop never returns.
func serveWait(srv *server.Server, wait func() error, shutdown func()) error {
	if !menubar.Supported() {
		return wait() // headless (ssh) session: no window server, no icon
	}
	go func() {
		if err := wait(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		os.Exit(0)
	}()

	var quitOnce sync.Once
	menubar.Run(menubar.Handlers{
		OpenUI: func() {
			go exec.Command("open", uiURL(srv.Config().Listen)).Run()
		},
		Restart: func() {
			go srv.RestartDaemon(0, srv.RunningVMNames(context.Background()))
		},
		QuitMessage: func() string {
			switch n := len(srv.RunningVMNames(context.Background())); n {
			case 0:
				return "The web UI, API and SSH gate go offline. No VMs are running."
			case 1:
				return "The web UI, API and SSH gate go offline, and 1 running VM shuts down (its disk is kept)."
			default:
				return fmt.Sprintf("The web UI, API and SSH gate go offline, and %d running VMs shut down (their disks are kept).", n)
			}
		},
		Quit: func() {
			quitOnce.Do(func() {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					if running := srv.RunningVMNames(ctx); len(running) > 0 {
						log.Printf("menu quit: stopping %s", strings.Join(running, ", "))
						srv.StopVMs(ctx, running)
					}
					shutdown()
					log.Printf("menu quit: daemon stopped")
					os.Exit(0)
				}()
			})
		},
	})
	return nil // unreachable — menubar.Run never returns
}

// uiURL turns the API listen address into something a local browser can open.
func uiURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}
