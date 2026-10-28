package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func run(t *testing.T, ctx context.Context, args ...string) (string, error) {
	t.Helper()
	cmd := RootCmd()
	var out bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func TestServe(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("x"), 0o644)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	out, err := run(t, ctx, "serve", "--dir", dir, "--addr", "127.0.0.1:0")
	if err != nil || !strings.HasPrefix(out, "http://127.0.0.1:") || !strings.Contains(out, "/playground/") {
		t.Fatalf("serve: %q %v", out, err)
	}
	old := listen
	listen = func(string, string) (net.Listener, error) { return nil, errors.New("no port") }
	defer func() { listen = old }()
	if _, err := run(t, context.Background(), "serve"); err == nil {
		t.Fatal("listen failure swallowed")
	}
}

func TestVerifyAndArgs(t *testing.T) {
	if _, err := run(t, context.Background(), "verify", "--manifest", filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Fatal("missing manifest accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte("{"), 0o644)
	if _, err := run(t, context.Background(), "verify", "--manifest", bad); err == nil {
		t.Fatal("bad manifest accepted")
	}
	empty := filepath.Join(t.TempDir(), "empty.json")
	os.WriteFile(empty, []byte(`{"build":"x","files":{}}`), 0o644)
	out, err := run(t, context.Background(), "verify", "--manifest", empty, "--url", "http://127.0.0.1:1/")
	if err != nil || !strings.Contains(out, "0 files") {
		t.Fatalf("empty manifest: %q %v", out, err)
	}
	if _, err := run(t, context.Background(), "playwright-driver"); err == nil {
		t.Fatal("--dir is required")
	}
	if _, err := run(t, context.Background(), "build", "extra"); err == nil {
		t.Fatal("build takes no arguments")
	}
}

func TestMainExit(t *testing.T) {
	var code int
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"site", "no-such-command"}
	main()
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
}
