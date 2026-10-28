package site

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGo stands in for the go command: it writes the wasm file and answers
// `go env GOROOT` with a directory holding a wasm_exec.js.
func fakeGo(t *testing.T, goroot string, fail string) {
	t.Helper()
	old := runGo
	runGo = func(ctx context.Context, dir string, env []string, stdout io.Writer, args ...string) error {
		if args[0] == fail {
			return errors.New("go " + fail + " failed")
		}
		switch args[0] {
		case "build":
			for i, a := range args {
				if a == "-o" {
					return os.WriteFile(args[i+1], []byte("\x00asm"), 0o644)
				}
			}
		case "env":
			io.WriteString(stdout, goroot+"\n")
		}
		return nil
	}
	t.Cleanup(func() { runGo = old })
}

func TestBuild(t *testing.T) {
	root := filepath.Join("..", "..")
	goroot := t.TempDir()
	os.MkdirAll(filepath.Join(goroot, "lib", "wasm"), 0o755)
	os.WriteFile(filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"), []byte("// go"), 0o644)
	fakeGo(t, goroot, "")
	out := filepath.Join(t.TempDir(), "site")
	var log bytes.Buffer
	m, err := Build(context.Background(), Options{Root: root, Out: out, Log: &log})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range append(webFiles, "fsplay.wasm", "wasm_exec.js") {
		if m.Files[f] == "" {
			t.Fatalf("%s missing from the manifest", f)
		}
	}
	b, err := os.ReadFile(filepath.Join(out, "build.json"))
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(b, &back); err != nil || back.Build != m.Build {
		t.Fatalf("build.json: %v %+v", err, back)
	}
	if !strings.Contains(log.String(), m.Build) {
		t.Fatalf("log: %s", log.String())
	}
	// Building twice gives the same build id.
	m2, err := Build(context.Background(), Options{Root: root, Out: out})
	if err != nil || m2.Build != m.Build {
		t.Fatalf("rebuild: %v %s != %s", err, m2.Build, m.Build)
	}
}

func TestBuildFailures(t *testing.T) {
	root := filepath.Join("..", "..")
	if _, err := Build(context.Background(), Options{Root: root}); err == nil {
		t.Fatal("no output directory accepted")
	}
	for _, step := range []string{"build", "env"} {
		fakeGo(t, t.TempDir(), step)
		if _, err := Build(context.Background(), Options{Root: root, Out: filepath.Join(t.TempDir(), "s")}); err == nil {
			t.Fatalf("go %s failure swallowed", step)
		}
	}
	fakeGo(t, t.TempDir(), "") // GOROOT without wasm_exec.js
	if _, err := Build(context.Background(), Options{Root: root, Out: filepath.Join(t.TempDir(), "s")}); err == nil {
		t.Fatal("missing wasm_exec.js accepted")
	}
	goroot := t.TempDir()
	os.MkdirAll(filepath.Join(goroot, "lib", "wasm"), 0o755)
	os.WriteFile(filepath.Join(goroot, "lib", "wasm", "wasm_exec.js"), []byte("//"), 0o644)
	fakeGo(t, goroot, "")
	if _, err := Build(context.Background(), Options{Root: t.TempDir(), Out: filepath.Join(t.TempDir(), "s")}); err == nil {
		t.Fatal("missing web files accepted")
	}
}

func TestRealGoCommand(t *testing.T) {
	var out bytes.Buffer
	if err := runGo(context.Background(), ".", nil, &out, "env", "GOOS"); err != nil || out.Len() == 0 {
		t.Fatalf("go env: %v %q", err, out.String())
	}
	if err := runGo(context.Background(), ".", nil, io.Discard, "no-such-subcommand"); err == nil {
		t.Fatal("a failing go command succeeded")
	}
}

func TestHandler(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("<p>hi</p>"), 0o644)
	os.WriteFile(filepath.Join(dir, "fsplay.wasm"), []byte("\x00asm"), 0o644)
	srv := httptest.NewServer(Handler(dir, "/playground/"))
	defer srv.Close()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cases := map[string]int{"/playground": 302, "/playground/": 200, "/playground/fsplay.wasm": 200, "/other": 404}
	for p, want := range cases {
		resp, err := c.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s: %d, want %d", p, resp.StatusCode, want)
		}
		if p == "/playground/fsplay.wasm" && resp.Header.Get("Content-Type") != "application/wasm" {
			t.Fatalf("wasm served as %q", resp.Header.Get("Content-Type"))
		}
	}
}

func TestVerify(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	m, err := manifestOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	if _, err := Verify(context.Background(), srv.URL, m, time.Millisecond, io.Discard); err != nil {
		t.Fatalf("matching site: %v", err)
	}
	// A file that differs keeps failing until the context ends.
	m.Files["a.txt"] = strings.Repeat("0", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var log bytes.Buffer
	if _, err := Verify(ctx, srv.URL+"/", m, 10*time.Millisecond, &log); err == nil || !strings.Contains(log.String(), "not yet") {
		t.Fatalf("differing site: %v %s", err, log.String())
	}
	// A missing file.
	m.Files = map[string]string{"gone.txt": strings.Repeat("0", 64)}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel2()
	if _, err := Verify(ctx2, srv.URL, m, 10*time.Millisecond, io.Discard); err == nil {
		t.Fatal("missing file verified")
	}
	if _, err := fetchSum(context.Background(), "http://127.0.0.1:1/x"); err == nil {
		t.Fatal("unreachable host fetched")
	}
	if _, err := fetchSum(context.Background(), "::bad"); err == nil {
		t.Fatal("bad URL fetched")
	}
	if short("abc") != "abc" {
		t.Fatal("short")
	}
	if _, err := manifestOf(filepath.Join(dir, "nope")); err == nil {
		t.Fatal("manifest of a missing dir")
	}
}
