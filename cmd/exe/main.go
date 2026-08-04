// Command exe is a personal VM cloud for your Mac: a daemon that runs
// Virtualization.framework VMs, a vibecoding agent backed by Ollama, and a
// Cloudflare-Tunnel-published reverse proxy — plus the CLI to drive it all.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"exe/internal/config"
	"exe/internal/keys"
	"exe/internal/proxy"
	"exe/internal/server"
	"exe/internal/vmm"
)

const usage = `exe — a personal VM cloud on this machine

Usage:
  exe serve                                run the daemon (API + HTTP proxy)
  exe init                                 write a starter config (~/.exe/config.json)
  exe ls                                   list VMs
  exe create <name> [-cpus N] [-mem MB] [-disk GB]
  exe start <name>
  exe stop <name>
  exe rm <name>
  exe ip <name>
  exe ssh <name> [command...]
  exe code <name> [-m model] <prompt...>   vibecode inside the VM (Ollama agent)
  exe expose <name> -port N [-sub name]    publish https://<sub>.<domain> -> VM port
  exe unexpose <host>                      remove a proxy route
  exe routes                               show proxy routes
`

func main() {
	log.SetFlags(log.Ltime)
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe()
	case "init":
		err = cmdInit()
	case "ls":
		err = cmdLs()
	case "create":
		err = cmdCreate(args)
	case "start":
		err = cmdSimpleVM(args, "start")
	case "stop":
		err = cmdSimpleVM(args, "stop")
	case "rm":
		err = cmdRm(args)
	case "ip":
		err = cmdIP(args)
	case "ssh":
		err = cmdSSH(args)
	case "code":
		err = cmdCode(args)
	case "expose":
		err = cmdExpose(args)
	case "unexpose":
		err = cmdUnexpose(args)
	case "routes":
		err = cmdRoutes()
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Printf("unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// ---- daemon ----------------------------------------------------------------

func cmdServe() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	stateDir := config.Dir()
	privKey, pubKey, err := keys.Ensure(stateDir)
	if err != nil {
		return err
	}
	mgr, err := vmm.New(vmm.Options{
		StateDir:      stateDir,
		ImageURL:      cfg.ImageURL,
		SSHUser:       cfg.SSHUser,
		AuthorizedKey: pubKey,
	})
	if err != nil {
		return err
	}
	px, err := proxy.New(filepath.Join(stateDir, "routes.json"))
	if err != nil {
		return err
	}
	srv := server.New(cfg, mgr, px, privKey, stateDir)

	if ie, ok := mgr.(vmm.ImageEnsurer); ok {
		go func() {
			if _, err := ie.EnsureImage(context.Background()); err != nil {
				log.Printf("base image prefetch failed: %v", err)
			}
		}()
	}

	apiSrv := &http.Server{Addr: cfg.Listen, Handler: srv.Handler()}
	proxySrv := &http.Server{Addr: cfg.ProxyListen, Handler: px.Handler()}
	errc := make(chan error, 2)
	go func() { errc <- apiSrv.ListenAndServe() }()
	go func() { errc <- proxySrv.ListenAndServe() }()
	log.Printf("exe daemon: API http://%s, proxy %s, state %s", displayAddr(cfg.Listen), cfg.ProxyListen, stateDir)
	log.Printf("note: VMs run inside this process and power off when it exits")

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	select {
	case err := <-errc:
		return err
	case <-sig:
		log.Printf("shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		apiSrv.Shutdown(ctx)
		proxySrv.Shutdown(ctx)
		return nil
	}
}

func cmdInit() error {
	p, err := config.WriteTemplate()
	if err != nil {
		return err
	}
	fmt.Printf(`wrote %s

Fill in:
  ollama.api_key           https://ollama.com/settings/keys (or export OLLAMA_API_KEY)
  cloudflare.api_token     token with Zone:DNS:Edit + Account:Cloudflare Tunnel:Edit
  cloudflare.account_id    dash.cloudflare.com -> right sidebar
  cloudflare.zone_id       zone overview page
  cloudflare.tunnel_id     Zero Trust -> Networks -> Tunnels (must be remotely managed)
  cloudflare.domain        e.g. apps.example.com or example.com
  advertise_host           this Mac's LAN or Tailscale IP, reachable from the tunnel host

Then: exe serve
`, p)
	return nil
}

// ---- API client helpers ------------------------------------------------------

func displayAddr(l string) string {
	if strings.HasPrefix(l, ":") {
		return "127.0.0.1" + l
	}
	return l
}

func api(cfg *config.Config, method, path string, body any, timeout time.Duration) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://"+displayAddr(cfg.Listen)+path, rd)
	if err != nil {
		return nil, err
	}
	if cfg.APIToken != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIToken)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		if strings.Contains(err.Error(), "connection refused") {
			return nil, fmt.Errorf("cannot reach the exe daemon at %s — is `exe serve` running?", displayAddr(cfg.Listen))
		}
		return nil, err
	}
	return resp, nil
}

func decodeInto(resp *http.Response, out any) error {
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, raw)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// splitName pulls the first non-flag argument out so flags may appear
// before or after the VM name.
func splitName(args []string) (string, []string, error) {
	for i, a := range args {
		if !strings.HasPrefix(a, "-") {
			rest := make([]string, 0, len(args)-1)
			rest = append(rest, args[:i]...)
			rest = append(rest, args[i+1:]...)
			return a, rest, nil
		}
	}
	return "", nil, fmt.Errorf("vm name required")
}

// ---- vm commands -------------------------------------------------------------

func printVMs(vms []*vmm.Info) {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATE\tCPUS\tMEM\tDISK\tIP")
	for _, v := range vms {
		ip := v.IP
		if ip == "" {
			ip = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%d\t%dMB\t%dGB\t%s\n", v.Name, v.State, v.CPUs, v.MemoryMB, v.DiskGB, ip)
	}
	tw.Flush()
}

func cmdLs() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "GET", "/v1/vms", nil, 30*time.Second)
	if err != nil {
		return err
	}
	var vms []*vmm.Info
	if err := decodeInto(resp, &vms); err != nil {
		return err
	}
	printVMs(vms)
	return nil
}

func cmdCreate(args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	cpus := fs.Int("cpus", 0, "vCPUs")
	mem := fs.Int("mem", 0, "memory MB")
	disk := fs.Int("disk", 0, "disk GB")
	fs.Parse(rest)
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	fmt.Printf("creating %s (first run may download the base image; watch `exe serve` logs)...\n", name)
	resp, err := api(cfg, "POST", "/v1/vms",
		vmm.Spec{Name: name, CPUs: *cpus, MemoryMB: *mem, DiskGB: *disk}, 45*time.Minute)
	if err != nil {
		return err
	}
	var info vmm.Info
	if err := decodeInto(resp, &info); err != nil {
		return err
	}
	printVMs([]*vmm.Info{&info})
	fmt.Printf("\nnext:\n  exe ssh %s\n  exe code %s \"build me ...\"\n  exe expose %s -port 8000\n", name, name, name)
	return nil
}

func cmdSimpleVM(args []string, action string) error {
	name, _, err := splitName(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "POST", "/v1/vms/"+name+"/"+action, map[string]string{}, 10*time.Minute)
	if err != nil {
		return err
	}
	if action == "start" {
		var info vmm.Info
		if err := decodeInto(resp, &info); err != nil {
			return err
		}
		printVMs([]*vmm.Info{&info})
		return nil
	}
	if err := decodeInto(resp, nil); err != nil {
		return err
	}
	fmt.Printf("%s: %s ok\n", name, action)
	return nil
}

func cmdRm(args []string) error {
	name, _, err := splitName(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "DELETE", "/v1/vms/"+name, nil, 2*time.Minute)
	if err != nil {
		return err
	}
	if err := decodeInto(resp, nil); err != nil {
		return err
	}
	fmt.Printf("%s: deleted\n", name)
	return nil
}

func getVM(cfg *config.Config, name string) (*vmm.Info, error) {
	resp, err := api(cfg, "GET", "/v1/vms/"+name, nil, 30*time.Second)
	if err != nil {
		return nil, err
	}
	var info vmm.Info
	if err := decodeInto(resp, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

func cmdIP(args []string) error {
	name, _, err := splitName(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	info, err := getVM(cfg, name)
	if err != nil {
		return err
	}
	if info.IP == "" {
		return fmt.Errorf("vm %s has no IP (state=%s)", name, info.State)
	}
	fmt.Println(info.IP)
	return nil
}

func cmdSSH(args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	info, err := getVM(cfg, name)
	if err != nil {
		return err
	}
	if info.IP == "" {
		return fmt.Errorf("vm %s has no IP (state=%s); `exe start %s` first", name, info.State, name)
	}
	keyPath := filepath.Join(config.Dir(), "ssh", "id_ed25519")
	sshArgs := []string{
		"-i", keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"-o", "LogLevel=ERROR",
		cfg.SSHUser + "@" + info.IP,
	}
	sshArgs = append(sshArgs, rest...)
	c := exec.Command("ssh", sshArgs...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

func cmdCode(args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("code", flag.ExitOnError)
	model := fs.String("m", "", "override model (e.g. glm-5.2, qwen3-coder)")
	fs.Parse(rest)
	prompt := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if prompt == "" {
		b, _ := io.ReadAll(os.Stdin)
		prompt = strings.TrimSpace(string(b))
	}
	if prompt == "" {
		return fmt.Errorf("prompt required: exe code %s \"build me ...\"", name)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "POST", "/v1/vms/"+name+"/agent",
		map[string]string{"prompt": prompt, "model": *model}, 0)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return decodeInto(resp, nil)
	}
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func cmdExpose(args []string) error {
	name, rest, err := splitName(args)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("expose", flag.ExitOnError)
	port := fs.Int("port", 0, "VM port to publish (required)")
	sub := fs.String("sub", "", "subdomain (default: vm name)")
	fs.Parse(rest)
	if *port == 0 {
		return fmt.Errorf("-port is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "POST", "/v1/vms/"+name+"/expose",
		map[string]any{"subdomain": *sub, "port": *port}, 2*time.Minute)
	if err != nil {
		return err
	}
	var res map[string]any
	if err := decodeInto(resp, &res); err != nil {
		return err
	}
	fmt.Printf("route: %s -> %s\n", res["host"], res["backend"])
	if v, ok := res["dns"]; ok {
		fmt.Println("dns:", v)
	}
	if v, ok := res["ingress"]; ok {
		fmt.Println("tunnel ingress ->", v)
	}
	if ws, ok := res["warnings"].([]any); ok {
		for _, wmsg := range ws {
			fmt.Println("warning:", wmsg)
		}
	}
	fmt.Println("url:", res["url"])
	return nil
}

func cmdUnexpose(args []string) error {
	host, _, err := splitName(args)
	if err != nil {
		return fmt.Errorf("host required, e.g. exe unexpose app.example.com")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "DELETE", "/v1/routes/"+host, nil, 30*time.Second)
	if err != nil {
		return err
	}
	if err := decodeInto(resp, nil); err != nil {
		return err
	}
	fmt.Printf("%s: route removed\n", host)
	return nil
}

func cmdRoutes() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	resp, err := api(cfg, "GET", "/v1/routes", nil, 30*time.Second)
	if err != nil {
		return err
	}
	var routes map[string]string
	if err := decodeInto(resp, &routes); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "HOST\tBACKEND")
	for h, b := range routes {
		fmt.Fprintf(tw, "%s\t%s\n", h, b)
	}
	tw.Flush()
	return nil
}
