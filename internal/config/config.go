// Package config loads the exe daemon/CLI configuration from ~/.exe/config.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const defaultImageURLTmpl = "https://cloud.debian.org/images/cloud/trixie/latest/debian-13-genericcloud-%s.raw"

type OllamaConfig struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key"`
	Model   string `json:"model"`
}

type CloudflareConfig struct {
	APIToken  string `json:"api_token"`
	AccountID string `json:"account_id"`
	ZoneID    string `json:"zone_id"`
	TunnelID  string `json:"tunnel_id"`
	Domain    string `json:"domain"`
}

type Config struct {
	// Listen is the API address. Bind to your Tailscale IP to reach the
	// daemon from other devices, e.g. "100.x.y.z:7777".
	Listen string `json:"listen"`
	// ProxyListen is the HTTP reverse-proxy address that the Cloudflare
	// tunnel (or anything else) forwards traffic to.
	ProxyListen string `json:"proxy_listen"`
	// AdvertiseHost is the address of THIS machine as reachable from the
	// cloudflared tunnel server (LAN IP or Tailscale IP).
	AdvertiseHost string `json:"advertise_host"`
	// APIToken, when set, is required as a Bearer token on every API call.
	APIToken string `json:"api_token"`

	SSHUser  string `json:"ssh_user"`
	ImageURL string `json:"image_url"`

	DefaultCPUs     int `json:"default_cpus"`
	DefaultMemoryMB int `json:"default_memory_mb"`
	DefaultDiskGB   int `json:"default_disk_gb"`

	Ollama     OllamaConfig     `json:"ollama"`
	Cloudflare CloudflareConfig `json:"cloudflare"`
}

// Dir returns the state directory (~/.exe, or $EXE_HOME).
func Dir() string {
	if d := os.Getenv("EXE_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".exe"
	}
	return filepath.Join(home, ".exe")
}

func Path() string { return filepath.Join(Dir(), "config.json") }

func Default() *Config {
	arch := "arm64"
	if runtime.GOARCH == "amd64" {
		arch = "amd64"
	}
	return &Config{
		Listen:          "127.0.0.1:7777",
		ProxyListen:     ":8090",
		SSHUser:         "dev",
		ImageURL:        fmt.Sprintf(defaultImageURLTmpl, arch),
		DefaultCPUs:     2,
		DefaultMemoryMB: 2048,
		DefaultDiskGB:   20,
		Ollama: OllamaConfig{
			BaseURL: "https://ollama.com",
			Model:   "glm-5.2",
		},
	}
}

// Load reads config.json over the defaults; secrets can also come from
// OLLAMA_API_KEY, CLOUDFLARE_API_TOKEN and EXE_API_TOKEN.
func Load() (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(Path())
	if err == nil {
		if err := json.Unmarshal(b, cfg); err != nil {
			return nil, fmt.Errorf("parse %s: %w", Path(), err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if v := os.Getenv("OLLAMA_API_KEY"); v != "" {
		cfg.Ollama.APIKey = v
	}
	if v := os.Getenv("CLOUDFLARE_API_TOKEN"); v != "" {
		cfg.Cloudflare.APIToken = v
	}
	if v := os.Getenv("EXE_API_TOKEN"); v != "" {
		cfg.APIToken = v
	}
	return cfg, nil
}

// WriteTemplate writes a starter config; it refuses to overwrite.
func WriteTemplate() (string, error) {
	p := Path()
	if _, err := os.Stat(p); err == nil {
		return p, fmt.Errorf("%s already exists", p)
	}
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		return p, err
	}
	b, _ := json.MarshalIndent(Default(), "", "  ")
	return p, os.WriteFile(p, append(b, '\n'), 0o600)
}
