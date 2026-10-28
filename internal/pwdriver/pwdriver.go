// Package pwdriver assembles the Playwright driver directory that
// playwright-go runs (package/cli.js, from npm's playwright-core), checked
// against the registry's sha512 integrity. playwright-go's own install step
// downloads driver zips the Playwright CDN no longer serves.
//
// Taken from go-wasmcontainers/playground (internal/thirdparty), which met
// the problem first.
package pwdriver

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// Registry is the npm registry; a test points it elsewhere.
var Registry = "https://registry.npmjs.org/"

func get(ctx context.Context, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s: more than %d bytes", url, max)
	}
	return b, err
}

// tarball fetches an npm package tarball and checks its sha512 integrity.
func tarball(ctx context.Context, name, version string) ([]byte, error) {
	meta, err := get(ctx, Registry+name+"/"+version, 4<<20)
	if err != nil {
		return nil, err
	}
	var m struct {
		Dist struct {
			Tarball   string `json:"tarball"`
			Integrity string `json:"integrity"`
		} `json:"dist"`
	}
	if err := json.Unmarshal(meta, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	alg, want, ok := strings.Cut(m.Dist.Integrity, "-")
	if !ok || alg != "sha512" {
		return nil, fmt.Errorf("%s: integrity %q is not sha512", name, m.Dist.Integrity)
	}
	if !strings.HasPrefix(m.Dist.Tarball, Registry) {
		return nil, fmt.Errorf("%s: tarball %q is off the registry", name, m.Dist.Tarball)
	}
	tgz, err := get(ctx, m.Dist.Tarball, 64<<20)
	if err != nil {
		return nil, err
	}
	sum := sha512.Sum512(tgz)
	if base64.StdEncoding.EncodeToString(sum[:]) != want {
		return nil, fmt.Errorf("%s: tarball does not match its integrity hash", name)
	}
	return tgz, nil
}

// Install writes playwright-core@version into dir/package.
func Install(ctx context.Context, dir, version string) error {
	tgz, err := tarball(ctx, "playwright-core", version)
	if err != nil {
		return err
	}
	zr, err := gzip.NewReader(bytes.NewReader(tgz))
	if err != nil {
		return err
	}
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		rel, ok := strings.CutPrefix(h.Name, "package/")
		if !ok || h.Typeflag != tar.TypeReg || strings.Contains(rel, "..") || filepath.IsAbs(rel) {
			continue
		}
		out := filepath.Join(dir, "package", filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(tr, 64<<20))
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, b, 0o644); err != nil {
			return err
		}
	}
}
