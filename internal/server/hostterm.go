package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"

	"github.com/coder/websocket"
	"github.com/creack/pty"
)

// handleHostTerminal bridges a browser WebSocket to a login shell on the
// host itself — the desktop's Terminal window. Same wire protocol as the
// VM terminal: binary frames carry terminal bytes both ways, text frames
// carry control messages ({"resize":[cols,rows]}).
func (s *Server) handleHostTerminal(w http.ResponseWriter, r *http.Request) {
	shell := os.Getenv("SHELL")
	if shell == "" {
		if runtime.GOOS == "darwin" {
			shell = "/bin/zsh"
		} else {
			shell = "/bin/sh"
		}
	}
	cmd := exec.Command(shell, "-l")
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: 80, Rows: 24})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	reap := func() {
		f.Close()
		cmd.Process.Kill()
		cmd.Wait()
	}

	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: []string{"*"}})
	if err != nil {
		reap()
		return
	}
	c.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer reap()
	defer c.CloseNow()

	out := &wsWriter{ctx: ctx, c: c}
	go func() {
		io.Copy(out, f) // pty output → browser; EOF when the shell exits
		c.Close(websocket.StatusNormalClosure, "session ended")
		cancel()
	}()

	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		switch typ {
		case websocket.MessageBinary:
			if _, err := f.Write(data); err != nil {
				return
			}
		case websocket.MessageText:
			var msg struct {
				Resize []int `json:"resize"`
			}
			if json.Unmarshal(data, &msg) == nil && len(msg.Resize) == 2 {
				pty.Setsize(f, &pty.Winsize{Cols: uint16(msg.Resize[0]), Rows: uint16(msg.Resize[1])})
			}
		}
	}
}
