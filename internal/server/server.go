// Package server exposes the exe daemon's HTTP API and embedded web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"exe/internal/agent"
	"exe/internal/cf"
	"exe/internal/config"
	"exe/internal/proxy"
	"exe/internal/sshexec"
	"exe/internal/transcript"
	"exe/internal/vmm"
)

type Server struct {
	VMs      vmm.Manager
	Proxy    *proxy.Proxy
	KeyPath  string
	StateDir string

	// OnRebind, when set, is called after a config PUT changes listen,
	// proxy_listen or ssh_listen so the daemon can re-bind those listeners
	// in-process — VMs live in this process and survive.
	OnRebind func(listen, proxyListen, sshListen string)

	cfg        atomic.Pointer[config.Config]
	activeRuns sync.Map // transcript id -> struct{}

	// Cached Cloudflare heartbeat so UI polling doesn't hammer the CF API.
	cfHealthMu  sync.Mutex
	cfHealthAt  time.Time
	cfHealthKey string
	cfHealthRes map[string]any
}

func New(cfg *config.Config, vms vmm.Manager, px *proxy.Proxy, keyPath, stateDir string) *Server {
	s := &Server{VMs: vms, Proxy: px, KeyPath: keyPath, StateDir: stateDir}
	s.cfg.Store(cfg)
	return s
}

// Config returns the live configuration (hot-swapped by PUT /v1/config).
func (s *Server) Config() *config.Config { return s.cfg.Load() }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /v1/vms", s.handleList)
	mux.HandleFunc("POST /v1/vms", s.handleCreate)
	mux.HandleFunc("GET /v1/vms/{name}", s.handleGet)
	mux.HandleFunc("POST /v1/vms/{name}/start", s.handleStart)
	mux.HandleFunc("POST /v1/vms/{name}/stop", s.handleStop)
	mux.HandleFunc("DELETE /v1/vms/{name}", s.handleDelete)
	mux.HandleFunc("POST /v1/vms/{name}/agent", s.handleAgent)
	mux.HandleFunc("POST /v1/vms/{name}/expose", s.handleExpose)
	mux.HandleFunc("GET /v1/vms/{name}/ports", s.handlePorts)
	mux.HandleFunc("GET /v1/vms/{name}/terminal", s.handleTerminal)
	mux.HandleFunc("GET /v1/vms/{name}/transcripts", s.handleTranscripts)
	mux.HandleFunc("GET /v1/vms/{name}/transcripts/{id}", s.handleTranscript)
	mux.HandleFunc("POST /v1/cloudflare/wizard", s.handleCFWizard)
	mux.HandleFunc("GET /v1/cloudflare/health", s.handleCFHealth)
	mux.HandleFunc("GET /v1/config", s.handleConfigGet)
	mux.HandleFunc("PUT /v1/config", s.handleConfigPut)
	mux.HandleFunc("POST /v1/daemon/restart", s.handleDaemonRestart)
	mux.HandleFunc("GET /v1/tailscale", s.handleTailscale)
	mux.HandleFunc("GET /v1/routes", s.handleRoutes)
	mux.HandleFunc("DELETE /v1/routes/{host}", s.handleRouteDelete)
	mux.Handle("GET /ui/", uiStatic)
	mux.HandleFunc("GET /", s.handleUI)
	return s.auth(mux)
}

// auth guards the API; the static UI page itself is public (it holds no
// data — every API call it makes carries the token).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tok := s.Config().APIToken; tok != "" && strings.HasPrefix(r.URL.Path, "/v1/") {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if got == "" {
				// Browsers cannot set headers on WebSocket connects.
				got = r.URL.Query().Get("token")
			}
			if got != tok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func errCode(err error) int {
	if errors.Is(err, vmm.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, vmm.ErrNotRunning) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	vms, err := s.VMs.List(r.Context())
	if err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	if vms == nil {
		vms = []*vmm.Info{}
	}
	writeJSON(w, http.StatusOK, vms)
}

// fillSpec applies the configured defaults to unset VM spec fields.
func (s *Server) fillSpec(spec *vmm.Spec) {
	cfg := s.Config()
	if spec.CPUs <= 0 {
		spec.CPUs = cfg.DefaultCPUs
	}
	if spec.MemoryMB <= 0 {
		spec.MemoryMB = cfg.DefaultMemoryMB
	}
	if spec.DiskGB <= 0 {
		spec.DiskGB = cfg.DefaultDiskGB
	}
}

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var spec vmm.Spec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	s.fillSpec(&spec)
	info, err := s.VMs.Create(r.Context(), spec)
	if err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	info, err := s.VMs.Get(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request) {
	info, err := s.VMs.Start(r.Context(), r.PathValue("name"))
	if err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) handleStop(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.Stop(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.VMs.Delete(r.Context(), r.PathValue("name")); err != nil {
		writeErr(w, errCode(err), err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (s *Server) runningVM(ctx context.Context, name string) (*vmm.Info, error) {
	info, err := s.VMs.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if info.State != "running" || info.IP == "" {
		return nil, fmt.Errorf("vm %s is not running with an IP (state=%s); start it first", name, info.State)
	}
	return info, nil
}

func (s *Server) transcriptDir(vm string) string {
	return filepath.Join(s.StateDir, "vms", vm, "transcripts")
}

// agentPrecheck validates a vibecode request and resolves the running VM.
func (s *Server) agentPrecheck(ctx context.Context, name, prompt string) (*vmm.Info, error) {
	cfg := s.Config()
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("prompt is required")
	}
	if cfg.Ollama.APIKey == "" && strings.Contains(cfg.Ollama.BaseURL, "ollama.com") {
		return nil, errors.New("ollama.api_key is not configured (or set OLLAMA_API_KEY)")
	}
	return s.runningVM(ctx, name)
}

// agentRun executes one vibecode run against info's VM, recording a
// transcript and streaming output through emit.
func (s *Server) agentRun(ctx context.Context, info *vmm.Info, name, prompt, model string, emit func(string)) error {
	cfg := s.Config()
	acfg := agent.Config{
		BaseURL: cfg.Ollama.BaseURL,
		APIKey:  cfg.Ollama.APIKey,
		Model:   cfg.Ollama.Model,
	}
	if model != "" {
		acfg.Model = model
	}
	rec, err := transcript.Start(s.transcriptDir(name), prompt, acfg.Model)
	if err != nil {
		return err
	}
	s.activeRuns.Store(rec.ID(), struct{}{})
	defer s.activeRuns.Delete(rec.ID())
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		rec.Append(line)
		emit(line)
	}
	target := sshexec.Target{Host: info.IP, User: cfg.SSHUser, KeyPath: s.KeyPath}
	logf("[agent] model %s on vm %s (%s)\n", acfg.Model, name, info.IP)
	runErr := agent.Run(ctx, acfg, target, name, prompt, logf)
	if runErr != nil {
		logf("\n[agent] ERROR: %v\n", runErr)
	} else {
		logf("\n[agent] done\n")
	}
	rec.Finish(runErr)
	return runErr
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	info, err := s.agentPrecheck(r.Context(), name, req.Prompt)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	s.agentRun(r.Context(), info, name, req.Prompt, req.Model, func(line string) {
		fmt.Fprint(w, line)
		if fl != nil {
			fl.Flush()
		}
	})
}

// exposeVM wires <sub>.<domain> through the local proxy to the VM's port
// and, when Cloudflare is configured, ensures the DNS record and tunnel
// ingress rule. The caller validates port and cloudflare.domain first.
func (s *Server) exposeVM(ctx context.Context, info *vmm.Info, name, sub string, port int) (map[string]any, error) {
	cfg := s.Config()
	if sub == "" {
		sub = name
	}
	fqdn := sub + "." + cfg.Cloudflare.Domain
	backend := "http://" + net.JoinHostPort(info.IP, strconv.Itoa(port))
	if err := s.Proxy.Set(fqdn, backend); err != nil {
		return nil, err
	}

	res := map[string]any{
		"host":    fqdn,
		"backend": backend,
		"url":     "https://" + fqdn,
	}
	var warnings []string
	cfc := &cf.Client{
		Token:     cfg.Cloudflare.APIToken,
		AccountID: cfg.Cloudflare.AccountID,
		ZoneID:    cfg.Cloudflare.ZoneID,
		TunnelID:  cfg.Cloudflare.TunnelID,
		Domain:    cfg.Cloudflare.Domain,
	}
	if cfc.Configured() {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := cfc.EnsureDNS(ctx, fqdn); err != nil {
			warnings = append(warnings, "dns: "+err.Error())
		} else {
			res["dns"] = "ok"
		}
		if cfg.AdvertiseHost == "" {
			warnings = append(warnings, "advertise_host not set; skipped tunnel ingress update")
		} else {
			proxyPort := cfg.ProxyListen
			if i := strings.LastIndex(proxyPort, ":"); i >= 0 {
				proxyPort = proxyPort[i+1:]
			}
			svc := "http://" + net.JoinHostPort(cfg.AdvertiseHost, proxyPort)
			if err := cfc.EnsureIngress(ctx, fqdn, svc); err != nil {
				warnings = append(warnings, "ingress: "+err.Error())
			} else {
				res["ingress"] = svc
			}
		}
	} else {
		warnings = append(warnings, "cloudflare not fully configured; only the local proxy route was added")
	}
	if len(warnings) > 0 {
		res["warnings"] = warnings
	}
	return res, nil
}

func (s *Server) handleExpose(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	name := r.PathValue("name")
	var req struct {
		Subdomain string `json:"subdomain"`
		Port      int    `json:"port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Port <= 0 || req.Port > 65535 {
		writeErr(w, http.StatusBadRequest, errors.New("port is required"))
		return
	}
	if cfg.Cloudflare.Domain == "" {
		writeErr(w, http.StatusBadRequest, errors.New("cloudflare.domain is not configured"))
		return
	}
	info, err := s.runningVM(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	res, err := s.exposeVM(r.Context(), info, name, req.Subdomain, req.Port)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// handleDaemonRestart restarts the daemon: running VMs are stopped (they
// live inside this process), a fresh copy of the binary is spawned with
// EXE_AUTOSTART naming them so it starts them again, and this process
// exits.
func (s *Server) handleDaemonRestart(w http.ResponseWriter, r *http.Request) {
	if _, err := os.Executable(); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	running := s.RunningVMNames(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"status": "restarting", "autostart": running})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
	go s.RestartDaemon(400*time.Millisecond, running) // delay lets the response reach the client
}

// RunningVMNames returns the names of the VMs currently in the running state.
func (s *Server) RunningVMNames(ctx context.Context) []string {
	var running []string
	if vms, err := s.VMs.List(ctx); err == nil {
		for _, v := range vms {
			if v.State == "running" {
				running = append(running, v.Name)
			}
		}
	}
	return running
}

// StopVMs stops the named VMs in parallel and waits for all of them.
func (s *Server) StopVMs(ctx context.Context, names []string) {
	var wg sync.WaitGroup
	for _, name := range names {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			if err := s.VMs.Stop(ctx, n); err != nil {
				log.Printf("stop %s: %v", n, err)
			}
		}(name)
	}
	wg.Wait()
}

// RestartDaemon stops the named VMs, spawns a fresh copy of the binary with
// EXE_AUTOSTART naming them so the new process starts them again, and exits
// this process. Spawn-then-exit rather than exec-in-place:
// Virtualization.framework keeps non-Go threads alive that wedge
// syscall.Exec's runtime hooks. Called from the restart API and the macOS
// menu bar icon.
func (s *Server) RestartDaemon(delay time.Duration, running []string) {
	exePath, err := os.Executable()
	if err != nil {
		log.Printf("restart: %v", err)
		return
	}
	time.Sleep(delay)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	s.StopVMs(ctx, running)
	cmd := exec.Command(exePath, os.Args[1:]...)
	cmd.Env = append(os.Environ(), "EXE_AUTOSTART="+strings.Join(running, ","))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		log.Printf("restart: spawn failed: %v", err)
		return
	}
	log.Printf("restart: handing over to pid %d (autostart: %s)", cmd.Process.Pid, strings.Join(running, ","))
	os.Exit(0)
}

// handleTailscale reports this host's Tailscale IPv4, detected as a CGNAT
// (100.64.0.0/10) address on a local interface — no tailscale CLI needed.
func (s *Server) handleTailscale(w http.ResponseWriter, r *http.Request) {
	res := map[string]any{"detected": false}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		_, cgnat, _ := net.ParseCIDR("100.64.0.0/10")
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok {
				if ip4 := ipn.IP.To4(); ip4 != nil && cgnat.Contains(ip4) {
					res = map[string]any{"detected": true, "ip": ip4.String()}
					break
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Proxy.Snapshot())
}

// removeRoute unpublishes a hostname: proxy route, Cloudflare DNS record,
// and tunnel ingress rule.
func (s *Server) removeRoute(ctx context.Context, host string) (map[string]any, error) {
	cfg := s.Config()
	if err := s.Proxy.Remove(host); err != nil {
		return nil, err
	}
	res := map[string]any{"status": "removed"}
	var warnings []string
	cfc := &cf.Client{
		Token:     cfg.Cloudflare.APIToken,
		AccountID: cfg.Cloudflare.AccountID,
		ZoneID:    cfg.Cloudflare.ZoneID,
		TunnelID:  cfg.Cloudflare.TunnelID,
		Domain:    cfg.Cloudflare.Domain,
	}
	if cfc.Configured() {
		ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		if err := cfc.DeleteDNS(ctx, host); err != nil {
			warnings = append(warnings, "dns: "+err.Error())
		} else {
			res["dns"] = "removed"
		}
		if err := cfc.RemoveIngress(ctx, host); err != nil {
			warnings = append(warnings, "ingress: "+err.Error())
		} else {
			res["ingress"] = "removed"
		}
	}
	if len(warnings) > 0 {
		res["warnings"] = warnings
	}
	return res, nil
}

func (s *Server) handleRouteDelete(w http.ResponseWriter, r *http.Request) {
	res, err := s.removeRoute(r.Context(), r.PathValue("host"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
