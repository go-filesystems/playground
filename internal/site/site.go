// Package site builds, serves and verifies the playground's static site: the
// page (web/), the Go engine compiled to WebAssembly, and the toolchain's
// wasm_exec.js that runs it.
package site

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Options configures Build.
type Options struct {
	Root string    // the module root
	Out  string    // the output directory, replaced
	Log  io.Writer // progress; nil discards
}

// Manifest lists a build's files and their SHA-256, so a deployment can be
// checked byte for byte.
type Manifest struct {
	Build string            `json:"build"`
	Files map[string]string `json:"files"`
}

// runGo is a seam for tests.
var runGo = func(ctx context.Context, dir string, env []string, stdout io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stdout = stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return nil
}

// webFiles are the page's files, copied as they are.
var webFiles = []string{"index.html", "app.js", "worker.js", "style.css"}

// Build assembles the site into o.Out and returns its manifest.
func Build(ctx context.Context, o Options) (*Manifest, error) {
	log := o.Log
	if log == nil {
		log = io.Discard
	}
	if o.Out == "" {
		return nil, fmt.Errorf("site: no output directory")
	}
	if err := os.RemoveAll(o.Out); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.Out, 0o755); err != nil {
		return nil, err
	}
	wasm, err := filepath.Abs(filepath.Join(o.Out, "fsplay.wasm"))
	if err != nil {
		return nil, err
	}
	fmt.Fprintln(log, "site: building the engine (GOOS=js GOARCH=wasm)")
	if err := runGo(ctx, o.Root, []string{"GOOS=js", "GOARCH=wasm", "GOWORK=off", "CGO_ENABLED=0"}, io.Discard,
		"build", "-trimpath", "-ldflags=-s -w", "-o", wasm, "./cmd/worker"); err != nil {
		return nil, err
	}
	var goroot bytes.Buffer
	if err := runGo(ctx, o.Root, []string{"GOWORK=off"}, &goroot, "env", "GOROOT"); err != nil {
		return nil, err
	}
	// wasm_exec.js must come from the toolchain that built fsplay.wasm.
	if err := copyFile(filepath.Join(strings.TrimSpace(goroot.String()), "lib", "wasm", "wasm_exec.js"),
		filepath.Join(o.Out, "wasm_exec.js")); err != nil {
		return nil, err
	}
	for _, f := range webFiles {
		if err := copyFile(filepath.Join(o.Root, "web", f), filepath.Join(o.Out, f)); err != nil {
			return nil, err
		}
	}
	if err := buildExplorer(ctx, o, log); err != nil {
		return nil, err
	}
	m, err := manifestOf(o.Out)
	if err != nil {
		return nil, err
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(o.Out, "build.json"), b, 0o644); err != nil {
		return nil, err
	}
	fmt.Fprintf(log, "site: %d files in %s (build %s)\n", len(m.Files), o.Out, m.Build)
	return m, nil
}

// explorerModule is the file explorer the page opens on demand, at the
// version go.mod's tool directive pins.
const explorerModule = "github.com/go-fileshare/explorer"

// explorerFiles are the explorer's page files, from its module's web/: its
// page becomes explorer.html beside the playground's; wasm_exec.js is shared,
// the same toolchain building both.
var explorerFiles = map[string]string{"index.html": "explorer.html", "boot.js": "boot.js", "explorer.css": "explorer.css"}

func buildExplorer(ctx context.Context, o Options, log io.Writer) error {
	fmt.Fprintln(log, "site: building the explorer (GOOS=js GOARCH=wasm)")
	wasm, err := filepath.Abs(filepath.Join(o.Out, "explorer.wasm"))
	if err != nil {
		return err
	}
	if err := runGo(ctx, o.Root, []string{"GOOS=js", "GOARCH=wasm", "GOWORK=off", "CGO_ENABLED=0"}, io.Discard,
		"build", "-trimpath", "-ldflags=-s -w", "-o", wasm, explorerModule); err != nil {
		return err
	}
	var dir bytes.Buffer
	if err := runGo(ctx, o.Root, []string{"GOWORK=off"}, &dir, "list", "-m", "-f", "{{.Dir}}", explorerModule); err != nil {
		return err
	}
	for from, to := range explorerFiles {
		if err := copyFile(filepath.Join(strings.TrimSpace(dir.String()), "web", from), filepath.Join(o.Out, to)); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(from, to string) error {
	b, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	return os.WriteFile(to, b, 0o644)
}

// manifestOf hashes every file in dir (not recursive; the site is flat).
func manifestOf(dir string) (*Manifest, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	m := &Manifest{Files: map[string]string{}}
	var names []string
	for _, e := range ents {
		if e.IsDir() || e.Name() == "build.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(b)
		m.Files[e.Name()] = hex.EncodeToString(sum[:])
		names = append(names, e.Name()+"="+m.Files[e.Name()])
	}
	sort.Strings(names)
	all := sha256.Sum256([]byte(strings.Join(names, "\n")))
	m.Build = hex.EncodeToString(all[:6])
	return m, nil
}

// Handler serves dir under prefix, as GitHub Pages serves the repository's
// site under /playground/. It adds the headers a static host may not, which
// the page does not depend on.
func Handler(dir, prefix string) http.Handler {
	prefix = strings.TrimSuffix(prefix, "/")
	files := http.StripPrefix(prefix, http.FileServer(http.Dir(dir)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == prefix {
			http.Redirect(w, r, prefix+"/", http.StatusFound)
			return
		}
		if !strings.HasPrefix(r.URL.Path, prefix+"/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		files.ServeHTTP(w, r)
	})
}

// Verify fetches every file of m from url until all match, the context ends,
// or a file keeps differing. It returns how long that took.
func Verify(ctx context.Context, url string, m *Manifest, every time.Duration, log io.Writer) (time.Duration, error) {
	t0 := time.Now()
	url = strings.TrimSuffix(url, "/") + "/"
	for {
		bad := ""
		for name, want := range m.Files {
			got, err := fetchSum(ctx, url+name)
			if err != nil || got != want {
				bad = fmt.Sprintf("%s: %v (want %s, got %s)", name, err, want[:12], short(got))
				break
			}
		}
		if bad == "" {
			return time.Since(t0), nil
		}
		fmt.Fprintln(log, "not yet:", bad)
		select {
		case <-ctx.Done():
			return time.Since(t0), fmt.Errorf("site: %s does not serve build %s: %s", url, m.Build, bad)
		case <-time.After(every):
		}
	}
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

var httpClient = &http.Client{Timeout: 60 * time.Second}

func fetchSum(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(resp.Body, 256<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
