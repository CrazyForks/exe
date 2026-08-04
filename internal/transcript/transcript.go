// Package transcript persists vibecoding agent runs: one .meta.json plus one
// .log file per run, stored under the VM's directory so they live and die
// with the VM.
package transcript

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Meta struct {
	ID        string     `json:"id"`
	Prompt    string     `json:"prompt"`
	Model     string     `json:"model"`
	Status    string     `json:"status"` // running | done | error (list may derive "interrupted")
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

type Rec struct {
	dir string

	mu   sync.Mutex
	meta Meta
	f    *os.File
}

func Start(dir, prompt, model string) (*Rec, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	suffix := make([]byte, 2)
	rand.Read(suffix)
	r := &Rec{
		dir: dir,
		meta: Meta{
			ID:        fmt.Sprintf("%d-%s", time.Now().UnixMilli(), hex.EncodeToString(suffix)),
			Prompt:    prompt,
			Model:     model,
			Status:    "running",
			StartedAt: time.Now().UTC(),
		},
	}
	if err := r.saveMeta(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, r.meta.ID+".log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	r.f = f
	return r, nil
}

func (r *Rec) ID() string { return r.meta.ID }

func (r *Rec) Append(s string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.f != nil {
		r.f.WriteString(s)
	}
}

// Finish records the outcome and closes the log.
func (r *Rec) Finish(runErr error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now().UTC()
	r.meta.EndedAt = &now
	if runErr != nil {
		r.meta.Status = "error"
		r.meta.Error = runErr.Error()
	} else {
		r.meta.Status = "done"
	}
	r.saveMeta()
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
}

func (r *Rec) saveMeta() error {
	b, _ := json.MarshalIndent(r.meta, "", "  ")
	return os.WriteFile(filepath.Join(r.dir, r.meta.ID+".meta.json"), append(b, '\n'), 0o644)
}

// List returns run metadata, newest first. A missing directory is empty.
func List(dir string) ([]Meta, error) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []Meta{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Meta{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".meta.json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var m Meta
		if json.Unmarshal(b, &m) == nil && m.ID != "" {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out, nil
}

// Load returns one run's metadata and full log text.
func Load(dir, id string) (*Meta, string, error) {
	if id == "" || strings.ContainsAny(id, "/\\.") {
		return nil, "", fmt.Errorf("bad transcript id")
	}
	b, err := os.ReadFile(filepath.Join(dir, id+".meta.json"))
	if err != nil {
		return nil, "", fmt.Errorf("transcript not found")
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, "", err
	}
	logb, err := os.ReadFile(filepath.Join(dir, id+".log"))
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	return &m, string(logb), nil
}
