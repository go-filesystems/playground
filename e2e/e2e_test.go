//go:build e2e

// Package e2e drives the playground in headless Chromium through Playwright:
// it makes the sample image, browses it, views and downloads files, opens an
// image whose file names are HTML, and (with E2E_IMAGE) measures open and
// list times on a real image with and without the block cache.
//
//	go run ./cmd/site playwright-driver --dir DIR
//	PLAYWRIGHT_DRIVER_PATH=DIR PLAYWRIGHT_NODEJS_PATH=$(command -v node) go test -tags e2e ./e2e/
//
// E2E_URL tests a deployed site instead of a local build; E2E_REPORT=<file>
// writes the measurements as JSON.
package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-fde/luks"
	filesystem_btrfs "github.com/go-filesystems/btrfs"
	filesystem_ext4 "github.com/go-filesystems/ext4"
	"github.com/playwright-community/playwright-go"

	"github.com/go-filesystems/playground/internal/site"
)

var siteDir string

func TestMain(m *testing.M) {
	if os.Getenv("E2E_URL") != "" {
		os.Exit(m.Run())
	}
	dir, err := os.MkdirTemp("", "e2e-site-")
	if err != nil {
		panic(err)
	}
	siteDir = filepath.Join(dir, "site")
	if _, err := site.Build(context.Background(), site.Options{Root: "..", Out: siteDir, Log: os.Stderr}); err != nil {
		fmt.Fprintln(os.Stderr, "site build:", err)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func baseURL(t *testing.T) string {
	if u := os.Getenv("E2E_URL"); u != "" {
		return u
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: site.Handler(siteDir, "/playground")}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "http://" + l.Addr().String() + "/playground/"
}

type browser struct {
	page    playwright.Page
	console []string
}

func launch(t *testing.T) *browser {
	t.Helper()
	pw, err := playwright.Run(&playwright.RunOptions{SkipInstallBrowsers: true})
	if err != nil {
		t.Fatalf("playwright: %v", err)
	}
	t.Cleanup(func() { pw.Stop() })
	b, err := pw.Chromium.Launch(playwright.BrowserTypeLaunchOptions{Headless: playwright.Bool(true)})
	if err != nil {
		t.Fatalf("chromium: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	ctx, err := b.NewContext(playwright.BrowserNewContextOptions{AcceptDownloads: playwright.Bool(true)})
	if err != nil {
		t.Fatal(err)
	}
	page, err := ctx.NewPage()
	if err != nil {
		t.Fatal(err)
	}
	br := &browser{page: page}
	page.On("console", func(m playwright.ConsoleMessage) { br.console = append(br.console, m.Text()) })
	page.On("pageerror", func(err error) { br.console = append(br.console, "pageerror: "+err.Error()) })
	return br
}

// until polls a JS predicate.
func (b *browser) until(t *testing.T, what, js string, d time.Duration) {
	t.Helper()
	t0 := time.Now()
	for {
		v, err := b.page.Evaluate(js)
		if err == nil && v == true {
			return
		}
		if time.Since(t0) > d {
			st, _ := b.page.Locator("#status").TextContent()
			t.Fatalf("%s: not after %s; status %q; console %q", what, d, st, b.console)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (b *browser) open(t *testing.T) {
	t.Helper()
	if _, err := b.page.Goto(baseURL(t)); err != nil {
		t.Fatal(err)
	}
	b.until(t, "engine ready", `() => !!(window.__fsplay && window.__fsplay.ready)`, 60*time.Second)
}

func text(t *testing.T, l playwright.Locator) string {
	t.Helper()
	s, err := l.TextContent()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSample(t *testing.T) {
	b := launch(t)
	b.open(t)
	p := b.page
	if err := p.Locator("#sample").Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "sample opened", `() => document.getElementById("fstype").textContent === "fat32"`, 30*time.Second)
	if got := text(t, p.Locator("#fslabel")); got != "PLAYGROUND" {
		t.Fatalf("label %q", got)
	}
	tree := text(t, p.Locator("#tree"))
	for _, want := range []string{"README.TXT", "docs", "images", "empty"} {
		if !strings.Contains(tree, want) {
			t.Fatalf("tree %q lacks %q", tree, want)
		}
	}

	// View a text file.
	if err := p.Locator("button.file", playwright.PageLocatorOptions{HasText: "README.TXT"}).Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "README shown", `() => (document.querySelector("#view pre.text")||{}).textContent?.includes("formatted in your browser")`, 15*time.Second)

	// Open a directory, view an image.
	if err := p.Locator("summary", playwright.PageLocatorOptions{HasText: "images"}).Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "images listed", `() => document.getElementById("tree").textContent.includes("gradient.png")`, 15*time.Second)
	if err := p.Locator("button.file", playwright.PageLocatorOptions{HasText: "gradient.png"}).Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "image decoded", `() => { const i = document.querySelector("#view img"); return !!i && i.complete && i.naturalWidth === 96 }`, 15*time.Second)

	// Download a file through the page.
	if err := p.Locator("summary", playwright.PageLocatorOptions{HasText: "docs"}).Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "docs listed", `() => document.getElementById("tree").textContent.includes("lines.txt")`, 15*time.Second)
	dl, err := p.ExpectDownload(func() error {
		return p.Locator(`button.dl[aria-label="download lines.txt"]`).Click()
	})
	if err != nil {
		t.Fatal(err)
	}
	if dl.SuggestedFilename() != "lines.txt" {
		t.Fatalf("download name %q", dl.SuggestedFilename())
	}
	path, err := dl.Path()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2000*len("line 0001: the quick brown fox jumps over the lazy dog\n") || !strings.HasPrefix(string(got), "line 0001:") {
		t.Fatalf("downloaded %d bytes", len(got))
	}
	for _, c := range b.console {
		if strings.HasPrefix(c, "pageerror") {
			t.Fatalf("page error: %s", c)
		}
	}
	shot(t, p, "sample")
}

// shot writes a screenshot to E2E_SHOTS, a directory that must lie outside
// every git work tree: a capture never belongs in a repository.
func shot(t *testing.T, p playwright.Page, name string) {
	dir := os.Getenv("E2E_SHOTS")
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			t.Fatalf("E2E_SHOTS %s is inside the git work tree %s", abs, d)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	for _, w := range []int{1280, 390} {
		p.SetViewportSize(w, 900)
		out := filepath.Join(abs, fmt.Sprintf("%s-%d.png", name, w))
		if _, err := p.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(out), FullPage: playwright.Bool(true)}); err != nil {
			t.Fatal(err)
		}
		t.Logf("screenshot: %s", out)
	}
}

// TestHostileNames opens an ext4 image whose file names are HTML. They must
// appear as text, and nothing in them may run.
func TestHostileNames(t *testing.T) {
	img := filepath.Join(t.TempDir(), "hostile.img")
	fsys, err := filesystem_ext4.Format(img, 16<<20, filesystem_ext4.FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	evil := `<img src=x onerror="window.__pwned=1">.txt`
	if err := fsys.WriteFile("/"+evil, []byte("boo"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/..hidden", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys.Close()

	b := launch(t)
	b.open(t)
	p := b.page
	if err := p.Locator("#pick").SetInputFiles(img); err != nil {
		t.Fatal(err)
	}
	b.until(t, "ext4 opened", `() => document.getElementById("fstype").textContent === "ext4"`, 30*time.Second)
	b.until(t, "names listed", `() => document.getElementById("tree").textContent.includes("onerror")`, 15*time.Second)
	if v, _ := p.Evaluate(`() => window.__pwned === undefined && document.querySelectorAll("#tree img").length === 0`); v != true {
		t.Fatal("a file name was parsed as HTML")
	}
	if v, _ := p.Evaluate(`() => [window.__fsplay.safeName("..hidden"), window.__fsplay.safeName("a/b\\c:d"), window.__fsplay.safeName("")]`); fmt.Sprint(v) != "[hidden a_b_c_d file]" {
		t.Fatalf("safeName: %v", v)
	}
}

// TestChooseKind opens a Btrfs image -- one of the disk-image drivers
// detect has no reader for -- by detection, forced as itself, and forced
// as something it is not.
func TestChooseKind(t *testing.T) {
	img := filepath.Join(t.TempDir(), "disk.img")
	fsys, err := filesystem_btrfs.Format(img, 16<<20, filesystem_btrfs.FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/hello.txt", []byte("hello btrfs"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys.Close()

	b := launch(t)
	b.open(t)
	p := b.page
	pick := func(kind string) {
		t.Helper()
		if _, err := p.Locator("#kind").SelectOption(playwright.SelectOptionValues{Values: &[]string{kind}}); err != nil {
			t.Fatal(err)
		}
		p.Evaluate(`() => { document.getElementById("fstype").textContent = ""; document.getElementById("status").classList.remove("error"); }`)
		if err := p.Locator("#pick").SetInputFiles(img); err != nil {
			t.Fatal(err)
		}
	}
	pick("auto")
	b.until(t, "btrfs detected", `() => document.getElementById("fstype").textContent === "btrfs"`, 30*time.Second)
	b.until(t, "file listed", `() => document.getElementById("tree").textContent.includes("hello.txt")`, 15*time.Second)
	if v, _ := p.Evaluate(`() => document.getElementById("fstype").title`); v != "detected" {
		t.Fatalf("title %v", v)
	}
	pick("ext4")
	b.until(t, "refused as ext4", `() => document.getElementById("status").classList.contains("error")`, 30*time.Second)
	pick("btrfs")
	b.until(t, "forced btrfs", `() => document.getElementById("fstype").textContent === "btrfs"`, 30*time.Second)
	if v, _ := p.Evaluate(`() => document.getElementById("fstype").title`); v != "chosen in the Filesystem list" {
		t.Fatalf("title %v", v)
	}
}

// fileDevice is a LUKS device's backing file.
type fileDevice struct{ *os.File }

// TestLUKS opens an ext4 image inside a LUKS1 container: the page asks for
// the password, refuses a wrong one, opens with the right one, and keeps
// neither.
func TestLUKS(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.img")
	fsys, err := filesystem_ext4.Format(inner, 16<<20, filesystem_ext4.FormatConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/secret.txt", []byte("inside luks"), 0o644); err != nil {
		t.Fatal(err)
	}
	fsys.Close()
	payload, err := os.ReadFile(inner)
	if err != nil {
		t.Fatal(err)
	}
	img := filepath.Join(dir, "disk.luks")
	f, err := os.Create(img)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(1 << 20); err != nil { // room for the header
		t.Fatal(err)
	}
	dev, err := luks.FormatOn(fileDevice{f}, []byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dev.WriteAt(payload, 0); err != nil {
		t.Fatal(err)
	}
	dev.Close()

	b := launch(t)
	b.open(t)
	p := b.page
	if err := p.Locator("#pick").SetInputFiles(img); err != nil {
		t.Fatal(err)
	}
	b.until(t, "password asked", `() => !document.getElementById("unlock").hidden && document.getElementById("status").textContent.includes("LUKS")`, 30*time.Second)

	submit := func(pw string) {
		t.Helper()
		if err := p.Locator("#password").Fill(pw); err != nil {
			t.Fatal(err)
		}
		if err := p.Locator("#unlock button").Click(); err != nil {
			t.Fatal(err)
		}
		if v, _ := p.Evaluate(`() => document.getElementById("password").value`); v != "" {
			t.Fatalf("the password stayed in the field: %v", v)
		}
	}
	submit("wrong")
	b.until(t, "wrong password refused", `() => document.getElementById("status").classList.contains("error")`, 60*time.Second)
	if v, _ := p.Evaluate(`() => document.getElementById("unlock").hidden`); v != false {
		t.Fatal("the password form went away after a wrong password")
	}
	submit("correct horse")
	b.until(t, "unlocked", `() => document.getElementById("fstype").textContent === "ext4 in LUKS"`, 60*time.Second)
	b.until(t, "file listed", `() => document.getElementById("tree").textContent.includes("secret.txt")`, 15*time.Second)
	if v, _ := p.Evaluate(`() => document.getElementById("unlock").hidden`); v != true {
		t.Fatal("the password form stayed after the unlock")
	}
}

// screenshot saves the page as name in E2E_SCREENSHOT_DIR, when set, to look
// at what the test saw. A directory inside a git work tree is refused: a
// capture one `git add -A` away from a public repository is how pictures get
// published by accident.
func screenshot(t *testing.T, p playwright.Page, name string) {
	t.Helper()
	dir := os.Getenv("E2E_SCREENSHOT_DIR")
	if dir == "" {
		return
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	for d := abs; ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			t.Fatalf("E2E_SCREENSHOT_DIR %s is inside the git work tree %s", abs, d)
		}
		if filepath.Dir(d) == d {
			break
		}
	}
	out := filepath.Join(abs, name)
	if _, err := p.Screenshot(playwright.PageScreenshotOptions{Path: playwright.String(out), FullPage: playwright.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	t.Logf("screenshot: %s", out)
}

// TestExplorer opens the sample in the go-fileshare explorer: the iframe
// loads it, it finds the bridge, lists the image's root through the engine's
// WebDAV, and closing it unloads the frame.
func TestExplorer(t *testing.T) {
	b := launch(t)
	b.open(t)
	p := b.page
	if err := p.Locator("#sample").Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "sample opened", `() => document.getElementById("fstype").textContent === "fat32"`, 30*time.Second)
	if err := p.Locator("#explore").Click(); err != nil {
		t.Fatal(err)
	}
	b.until(t, "explorer started", `() => {
		const d = document.getElementById("explorerframe").contentDocument;
		return !!d && !!d.getElementById("screen") && d.getElementById("screen").style.display === "block";
	}`, 120*time.Second)
	b.until(t, "root listed over the bridge", `() => window.__fsplay.dav.some((c) => c.method === "PROPFIND" && c.path === "/" && c.status === 207)`, 60*time.Second)
	// What the explorer shows, not only what the engine answered: a listing
	// the explorer could not read was answered 207 all the same.
	b.until(t, "the explorer lists the root", `() => {
		const s = window.__fsplay.explorer;
		return !!s && s.dir === "/" && (s.entries || []).includes("README.TXT") && s.status === "";
	}`, 60*time.Second)
	screenshot(t, p, "explorer.png")
	if err := p.Locator("#explorerclose").Click(); err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Evaluate(`() => document.getElementById("explorerbox").hidden && document.getElementById("explorerframe").getAttribute("src") === "about:blank"`); v != true {
		t.Fatal("closing did not unload the explorer")
	}
}

func TestUnknownImage(t *testing.T) {
	junk := filepath.Join(t.TempDir(), "junk.bin")
	os.WriteFile(junk, make([]byte, 1<<20), 0o644)
	b := launch(t)
	b.open(t)
	if err := b.page.Locator("#pick").SetInputFiles(junk); err != nil {
		t.Fatal(err)
	}
	b.until(t, "refusal shown", `() => document.getElementById("status").classList.contains("error")`, 30*time.Second)
	if st := text(t, b.page.Locator("#status")); !strings.Contains(st, "unknown filesystem") {
		t.Fatalf("status %q", st)
	}
}

// Measurement is one open of an image.
type Measurement struct {
	Cache        bool    `json:"cache"`
	OpenEngineMs float64 `json:"openEngineMs"`
	OpenTotalMs  float64 `json:"openEndToEndMs"`
	ListRootMs   float64 `json:"listRootMs"`
	BackendReads float64 `json:"backendReads"`
	BackendBytes float64 `json:"backendBytes"`
}

// TestMeasureCache opens E2E_IMAGE (or a generated ext4 image) with and
// without the block cache, in interleaved rounds, and reports the medians.
func TestMeasureCache(t *testing.T) {
	img := os.Getenv("E2E_IMAGE")
	if img == "" {
		img = filepath.Join(t.TempDir(), "gen.img")
		fsys, err := filesystem_ext4.Format(img, 64<<20, filesystem_ext4.FormatConfig{})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 300; i++ {
			fsys.WriteFile(fmt.Sprintf("/file-%03d.txt", i), []byte(strings.Repeat("x", i*10)), 0o644)
		}
		fsys.Close()
	}
	rounds := 5
	b := launch(t)
	byMode := map[bool][]Measurement{}
	for r := 0; r < rounds; r++ {
		for _, cache := range []bool{r%2 == 0, r%2 != 0} {
			b.open(t)
			if err := b.page.Locator("#cache").SetChecked(cache); err != nil {
				t.Fatal(err)
			}
			if err := b.page.Locator("#pick").SetInputFiles(img); err != nil {
				t.Fatal(err)
			}
			b.until(t, "opened", `() => !!(window.__fsplay.timings.listRoot >= 0 && window.__fsplay.info)`, 120*time.Second)
			v, err := b.page.Evaluate(`async () => {
				const t = window.__fsplay.timings;
				const s = (await window.__fsplay.req({op: "stats"})).stats;
				return {openEngineMs: t.open.engineMs, openEndToEndMs: t.open.endToEndMs, listRootMs: t.listRoot,
					backendReads: s.backendReads, backendBytes: s.backendBytes};
			}`)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(v)
			var m Measurement
			json.Unmarshal(raw, &m)
			m.Cache = cache
			byMode[cache] = append(byMode[cache], m)
		}
	}
	median := func(ms []Measurement, f func(Measurement) float64) float64 {
		v := make([]float64, len(ms))
		for i, m := range ms {
			v[i] = f(m)
		}
		sort.Float64s(v)
		return v[len(v)/2]
	}
	report := map[string]any{"image": filepath.Base(img), "rounds": rounds}
	for _, cache := range []bool{true, false} {
		ms := byMode[cache]
		key := map[bool]string{true: "cache", false: "nocache"}[cache]
		report[key] = map[string]float64{
			"openEngineMs":   median(ms, func(m Measurement) float64 { return m.OpenEngineMs }),
			"openEndToEndMs": median(ms, func(m Measurement) float64 { return m.OpenTotalMs }),
			"listRootMs":     median(ms, func(m Measurement) float64 { return m.ListRootMs }),
			"backendReads":   median(ms, func(m Measurement) float64 { return m.BackendReads }),
			"backendBytes":   median(ms, func(m Measurement) float64 { return m.BackendBytes }),
		}
	}
	out, _ := json.MarshalIndent(report, "", "  ")
	t.Logf("measurements:\n%s", out)
	if f := os.Getenv("E2E_REPORT"); f != "" {
		os.WriteFile(f, out, 0o644)
	}
	_ = io.Discard
}
