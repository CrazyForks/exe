package server

import (
	"context"
	"embed"
	_ "embed"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"exe/internal/config"
	"exe/internal/sshexec"
	"exe/internal/transcript"
)

//go:embed ui/index.html
var uiHTML []byte

//go:embed all:ui
var uiFS embed.FS

// uiStatic serves the vendored UI assets (xterm.js etc.) at /ui/.
var uiStatic = func() http.Handler {
	sub, err := fs.Sub(uiFS, "ui")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/ui/", http.FileServerFS(sub))
}()

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(uiHTML)
}

var ssProcessRE = regexp.MustCompile(`users:\(\("([^"]+)"`)

// handlePorts lists TCP ports listening on non-loopback addresses inside the
// VM (SSH excluded) so the UI can offer one-click links to running services.
func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	name := r.PathValue("name")
	info, err := s.runningVM(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	target := sshexec.Target{Host: info.IP, User: cfg.SSHUser, KeyPath: s.KeyPath}
	out, code, err := target.Run(ctx, `sudo -n ss -tlnp 2>/dev/null || ss -tln`, 65536)
	if err != nil || code != 0 {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	type svc struct {
		Port    int    `json:"port"`
		Process string `json:"process,omitempty"`
	}
	seen := map[int]string{}
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "LISTEN") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		addr := fields[3]
		i := strings.LastIndexByte(addr, ':')
		if i < 0 {
			continue
		}
		host := addr[:i]
		port, err := strconv.Atoi(addr[i+1:])
		if err != nil || port == 22 {
			continue
		}
		if strings.HasPrefix(host, "127.") || strings.HasPrefix(host, "[::1]") ||
			strings.Contains(host, "%lo") || strings.HasPrefix(host, "127.0.0.53%") {
			continue
		}
		proc := ""
		if m := ssProcessRE.FindStringSubmatch(line); m != nil {
			proc = m[1]
		}
		if strings.HasPrefix(proc, "systemd-") {
			continue
		}
		if cur, ok := seen[port]; !ok || cur == "" {
			seen[port] = proc
		}
	}
	services := []svc{}
	for port, proc := range seen {
		services = append(services, svc{Port: port, Process: proc})
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Port < services[j].Port })
	writeJSON(w, http.StatusOK, map[string]any{"ip": info.IP, "ports": services})
}

func (s *Server) handleTranscripts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := s.VMs.Get(r.Context(), name); err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	metas, err := transcript.List(s.transcriptDir(name))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// A run marked "running" whose recorder is gone died with a previous
	// daemon process or a dropped connection.
	for i, m := range metas {
		if m.Status == "running" {
			if _, active := s.activeRuns.Load(m.ID); !active {
				metas[i].Status = "interrupted"
			}
		}
	}
	writeJSON(w, http.StatusOK, metas)
}

func (s *Server) handleTranscript(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	meta, logText, err := transcript.Load(s.transcriptDir(name), r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if meta.Status == "running" {
		if _, active := s.activeRuns.Load(meta.ID); !active {
			meta.Status = "interrupted"
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta, "log": logText})
}

func (s *Server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Config())
}

// handleConfigPut validates, persists, and hot-swaps the configuration.
// Fields the daemon only reads at startup are reported in restart_required.
func (s *Server) handleConfigPut(w http.ResponseWriter, r *http.Request) {
	old := s.Config()
	nc := *old // unknown-in-request fields keep their current values
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&nc); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b, err := json.MarshalIndent(nc, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := os.WriteFile(config.Path(), append(b, '\n'), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.cfg.Store(&nc)

	var restart []string
	for _, f := range []struct{ name, oldV, newV string }{
		{"ssh_user", old.SSHUser, nc.SSHUser},
		{"image_url", old.ImageURL, nc.ImageURL},
	} {
		if f.oldV != f.newV {
			restart = append(restart, f.name)
		}
	}
	var rebinding []string
	if old.Listen != nc.Listen {
		rebinding = append(rebinding, "listen")
	}
	if old.ProxyListen != nc.ProxyListen {
		rebinding = append(rebinding, "proxy_listen")
	}
	res := map[string]any{"status": "saved"}
	if len(rebinding) > 0 {
		if s.OnRebind != nil {
			s.OnRebind(nc.Listen, nc.ProxyListen)
			res["rebinding"] = rebinding
		} else {
			restart = append(restart, rebinding...)
		}
	}
	if restart != nil {
		res["restart_required"] = restart
	}
	writeJSON(w, http.StatusOK, res)
}
