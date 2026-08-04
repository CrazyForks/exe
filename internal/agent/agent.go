// Package agent implements a vibecoding loop: an Ollama (cloud or local)
// model drives bash / write_file / read_file tools over SSH inside a VM.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"exe/internal/sshexec"
)

type Config struct {
	BaseURL string
	APIKey  string
	Model   string
}

type Logf func(format string, args ...any)

const (
	maxTurns      = 60
	maxToolOutput = 12000
	toolTimeout   = 5 * time.Minute
)

const systemPromptTmpl = `You are exe-agent, an autonomous coding agent operating a Debian Linux VM named %s.
You are connected over SSH as user %s, who has passwordless sudo.

Rules:
- Use the bash tool to inspect and change the system. Install packages with: sudo apt-get install -y <pkg> (run sudo apt-get update once first).
- Build the project under ~/app unless the user says otherwise.
- If the deliverable is a web app or service: bind it to 0.0.0.0, and install a systemd unit (write /etc/systemd/system/app.service via sudo tee, then sudo systemctl enable --now app) so it keeps running after you finish.
- Servers must handle concurrent connections: browsers hold idle preconnections open, which wedges single-threaded servers. With Python's stdlib use ThreadingHTTPServer, never plain HTTPServer.
- Verify your work before finishing (e.g. curl -s http://localhost:PORT).
- When everything works, reply WITHOUT any tool call: a short summary of what you built and the port the service listens on.`

type message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	Function struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"function"`
}

type toolFunc struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type tool struct {
	Type     string   `json:"type"`
	Function toolFunc `json:"function"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []tool    `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
}

type chatResponse struct {
	Message message `json:"message"`
	Error   string  `json:"error,omitempty"`
}

func mkTool(name, desc string, props map[string]any, required []string) tool {
	return tool{Type: "function", Function: toolFunc{
		Name:        name,
		Description: desc,
		Parameters: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		},
	}}
}

// Run executes the agent loop until the model stops calling tools.
func Run(ctx context.Context, cfg Config, target sshexec.Target, vmName, prompt string, logf Logf) error {
	tools := []tool{
		mkTool("bash", "Run a shell command on the VM; returns combined output and exit code.", map[string]any{
			"command": map[string]any{"type": "string", "description": "shell command to run"},
		}, []string{"command"}),
		mkTool("write_file", "Create or overwrite a file on the VM; parent directories are created.", map[string]any{
			"path":    map[string]any{"type": "string"},
			"content": map[string]any{"type": "string"},
		}, []string{"path", "content"}),
		mkTool("read_file", "Read a text file from the VM.", map[string]any{
			"path": map[string]any{"type": "string"},
		}, []string{"path"}),
	}
	msgs := []message{
		{Role: "system", Content: fmt.Sprintf(systemPromptTmpl, vmName, target.User)},
		{Role: "user", Content: prompt},
	}
	for turn := 0; turn < maxTurns; turn++ {
		resp, err := chat(ctx, cfg, msgs, tools)
		if err != nil {
			return err
		}
		msgs = append(msgs, resp.Message)
		if c := strings.TrimSpace(resp.Message.Content); c != "" {
			logf("\n%s\n", c)
		}
		if len(resp.Message.ToolCalls) == 0 {
			return nil
		}
		for _, tc := range resp.Message.ToolCalls {
			result := execTool(ctx, target, tc, logf)
			msgs = append(msgs, message{Role: "tool", ToolName: tc.Function.Name, Content: result})
		}
	}
	return fmt.Errorf("agent stopped after %d turns without finishing", maxTurns)
}

// Version reports the Ollama server's version (GET /api/version).
func Version(ctx context.Context, cfg Config) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(cfg.BaseURL, "/")+"/api/version", nil)
	if err != nil {
		return "", err
	}
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ollama version: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Version == "" {
		return "", fmt.Errorf("parse ollama version: %s", sshexec.Truncate(string(raw), 200))
	}
	return out.Version, nil
}

func chat(ctx context.Context, cfg Config, msgs []message, tools []tool) (*chatResponse, error) {
	body, err := json.Marshal(chatRequest{Model: cfg.Model, Messages: msgs, Tools: tools, Stream: false})
	if err != nil {
		return nil, err
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost,
		strings.TrimRight(cfg.BaseURL, "/")+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama %s: HTTP %d: %s", cfg.Model, resp.StatusCode, sshexec.Truncate(string(raw), 2000))
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse ollama response: %w: %s", err, sshexec.Truncate(string(raw), 2000))
	}
	if out.Error != "" {
		return nil, fmt.Errorf("ollama: %s", out.Error)
	}
	return &out, nil
}

func execTool(ctx context.Context, target sshexec.Target, tc toolCall, logf Logf) string {
	args := parseArgs(tc.Function.Arguments)
	str := func(k string) string { v, _ := args[k].(string); return v }
	tctx, cancel := context.WithTimeout(ctx, toolTimeout)
	defer cancel()

	switch tc.Function.Name {
	case "bash":
		cmd := str("command")
		logf("$ %s\n", cmd)
		out, code, err := target.Run(tctx, cmd, maxToolOutput)
		if err != nil {
			out = fmt.Sprintf("error: %v\n%s", err, out)
		}
		if strings.TrimSpace(out) != "" {
			logf("%s\n", strings.TrimRight(out, "\n"))
		}
		if code != 0 {
			out += fmt.Sprintf("\n[exit code %d]", code)
		} else if strings.TrimSpace(out) == "" {
			out = "(no output, exit code 0)"
		}
		return out
	case "write_file":
		p := str("path")
		content := str("content")
		logf("[write %s, %d bytes]\n", p, len(content))
		if err := target.WriteFile(tctx, p, []byte(content)); err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return "ok"
	case "read_file":
		p := str("path")
		logf("[read %s]\n", p)
		out, err := target.ReadFile(tctx, p, maxToolOutput)
		if err != nil {
			return fmt.Sprintf("error: %v", err)
		}
		return out
	default:
		return fmt.Sprintf("unknown tool %q", tc.Function.Name)
	}
}

// parseArgs tolerates both a JSON object and a JSON-encoded string of one.
func parseArgs(raw json.RawMessage) map[string]any {
	args := map[string]any{}
	if len(raw) == 0 {
		return args
	}
	if err := json.Unmarshal(raw, &args); err != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			json.Unmarshal([]byte(s), &args)
		}
	}
	return args
}
