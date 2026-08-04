// Package server exposes the exe daemon's HTTP API and embedded web UI.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	var spec vmm.Spec
	if err := json.NewDecoder(r.Body).Decode(&spec); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if spec.CPUs <= 0 {
		spec.CPUs = cfg.DefaultCPUs
	}
	if spec.MemoryMB <= 0 {
		spec.MemoryMB = cfg.DefaultMemoryMB
	}
	if spec.DiskGB <= 0 {
		spec.DiskGB = cfg.DefaultDiskGB
	}
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

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	cfg := s.Config()
	name := r.PathValue("name")
	var req struct {
		Prompt string `json:"prompt"`
		Model  string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("prompt is required"))
		return
	}
	if cfg.Ollama.APIKey == "" && strings.Contains(cfg.Ollama.BaseURL, "ollama.com") {
		writeErr(w, http.StatusBadRequest, errors.New("ollama.api_key is not configured (or set OLLAMA_API_KEY)"))
		return
	}
	info, err := s.runningVM(r.Context(), name)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}

	acfg := agent.Config{
		BaseURL: cfg.Ollama.BaseURL,
		APIKey:  cfg.Ollama.APIKey,
		Model:   cfg.Ollama.Model,
	}
	if req.Model != "" {
		acfg.Model = req.Model
	}

	rec, err := transcript.Start(s.transcriptDir(name), req.Prompt, acfg.Model)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.activeRuns.Store(rec.ID(), struct{}{})
	defer s.activeRuns.Delete(rec.ID())

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		rec.Append(line)
		fmt.Fprint(w, line)
		if fl != nil {
			fl.Flush()
		}
	}

	target := sshexec.Target{Host: info.IP, User: cfg.SSHUser, KeyPath: s.KeyPath}
	logf("[agent] model %s on vm %s (%s)\n", acfg.Model, name, info.IP)
	runErr := agent.Run(r.Context(), acfg, target, name, req.Prompt, logf)
	if runErr != nil {
		logf("\n[agent] ERROR: %v\n", runErr)
	} else {
		logf("\n[agent] done\n")
	}
	rec.Finish(runErr)
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
	sub := req.Subdomain
	if sub == "" {
		sub = name
	}
	fqdn := sub + "." + cfg.Cloudflare.Domain
	backend := "http://" + net.JoinHostPort(info.IP, strconv.Itoa(req.Port))
	if err := s.Proxy.Set(fqdn, backend); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
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
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
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
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) handleRoutes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Proxy.Snapshot())
}

// handleRouteDelete removes a proxy route. The Cloudflare DNS record and
// ingress rule, if any, are left in place; the host then 502s at the proxy.
func (s *Server) handleRouteDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.Proxy.Remove(r.PathValue("host")); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}
