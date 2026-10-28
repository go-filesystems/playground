package pwdriver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func tgz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.WriteHeader(&tar.Header{Name: "package/dir/", Typeflag: tar.TypeDir, Mode: 0o755})
	tw.Close()
	zw.Close()
	return buf.Bytes()
}

// registry serves one package version whose integrity is sum (or the
// tarball's own when sum is "").
func registry(t *testing.T, tarball []byte, integrity func(string) string, tarURL func(string) string) {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/playwright-core/1.0.0":
			s := sha512.Sum512(tarball)
			fmt.Fprintf(w, `{"dist":{"tarball":%q,"integrity":%q}}`,
				tarURL(srv.URL+"/"), integrity("sha512-"+base64.StdEncoding.EncodeToString(s[:])))
		case "/t.tgz":
			w.Write(tarball)
		case "/bad":
			w.Write([]byte("{"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := Registry
	Registry = srv.URL + "/"
	t.Cleanup(func() { Registry = old })
}

func same(s string) string { return s }

func TestInstall(t *testing.T) {
	tb := tgz(t, map[string]string{"package/cli.js": "// cli", "package/lib/x.js": "x", "package/../evil": "no", "other/y": "no"})
	registry(t, tb, same, func(u string) string { return u + "t.tgz" })
	dir := t.TempDir()
	if err := Install(context.Background(), dir, "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "package", "cli.js")); err != nil || string(b) != "// cli" {
		t.Fatalf("cli.js: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "evil")); err == nil {
		t.Fatal("a path escaping package/ was written")
	}
}

func TestInstallRefusals(t *testing.T) {
	tb := tgz(t, map[string]string{"package/cli.js": "x"})
	cases := map[string]struct {
		integrity func(string) string
		url       func(string) string
	}{
		"wrong hash":    {func(string) string { return "sha512-AAAA" }, func(u string) string { return u + "t.tgz" }},
		"not sha512":    {func(string) string { return "sha1-AAAA" }, func(u string) string { return u + "t.tgz" }},
		"off registry":  {same, func(string) string { return "https://example.com/t.tgz" }},
		"missing tgz":   {same, func(u string) string { return u + "none.tgz" }},
		"not a tarball": {same, func(u string) string { return u + "bad" }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			registry(t, tb, c.integrity, c.url)
			if err := Install(context.Background(), t.TempDir(), "1.0.0"); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	registry(t, tb, same, same)
	if err := Install(context.Background(), t.TempDir(), "9.9.9"); err == nil {
		t.Fatal("missing version accepted")
	}
	Registry = "http://127.0.0.1:1/"
	if err := Install(context.Background(), t.TempDir(), "1.0.0"); err == nil {
		t.Fatal("unreachable registry accepted")
	}
}
