// Command site builds, serves and verifies the playground's static site.
//
//	site build  --out _site
//	site serve  --dir _site [--addr 127.0.0.1:8080] [--prefix /playground]
//	site verify --url https://go-filesystems.github.io/playground/ --manifest _site/build.json
//	site playwright-driver --dir DIR
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-filesystems/playground/internal/pwdriver"
	"github.com/go-filesystems/playground/internal/site"
)

var osExit = os.Exit

// listen is a seam for tests.
var listen = net.Listen

func main() {
	cmd := RootCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "site:", err)
		osExit(1)
	}
}

// RootCmd builds the command tree.
func RootCmd() *cobra.Command {
	root := &cobra.Command{Use: "site", Short: "Build, serve and verify the go-filesystems playground", SilenceUsage: true, SilenceErrors: true}

	var o site.Options
	build := &cobra.Command{
		Use:   "build",
		Short: "Assemble the site: the page, the engine (wasm) and wasm_exec.js",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if o.Root == "" {
				wd, err := os.Getwd()
				if err != nil {
					return err
				}
				o.Root = wd
			}
			o.Log = cmd.OutOrStdout()
			_, err := site.Build(cmd.Context(), o)
			return err
		},
	}
	build.Flags().StringVarP(&o.Out, "out", "o", "_site", "output directory (replaced)")
	build.Flags().StringVar(&o.Root, "root", "", "module root (default: the working directory)")

	var dir, addr, prefix string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve a built site locally under --prefix",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			l, err := listen("tcp", addr)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "http://%s%s/\n", l.Addr(), prefix)
			srv := &http.Server{Handler: site.Handler(dir, prefix), ReadHeaderTimeout: 10 * time.Second}
			go func() {
				<-cmd.Context().Done()
				srv.Close()
			}()
			if err := srv.Serve(l); err != http.ErrServerClosed {
				return err
			}
			return nil
		},
	}
	serve.Flags().StringVarP(&dir, "dir", "d", "_site", "site directory")
	serve.Flags().StringVarP(&addr, "addr", "a", "127.0.0.1:8080", "listen address (loopback)")
	serve.Flags().StringVar(&prefix, "prefix", "/playground", "path prefix, as on GitHub Pages")

	var url, manifest string
	var timeout, every time.Duration
	verify := &cobra.Command{
		Use:   "verify",
		Short: "Wait until a deployed site serves exactly the files of a build",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := os.ReadFile(manifest)
			if err != nil {
				return err
			}
			var m site.Manifest
			if err := json.Unmarshal(b, &m); err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			took, err := site.Verify(ctx, url, &m, every, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "live: build %s, %d files byte-identical, after %s\n", m.Build, len(m.Files), took.Round(time.Second))
			return nil
		},
	}
	verify.Flags().StringVar(&url, "url", "https://go-filesystems.github.io/playground/", "deployed site")
	verify.Flags().StringVar(&manifest, "manifest", "_site/build.json", "the build's manifest")
	verify.Flags().DurationVar(&timeout, "timeout", 20*time.Minute, "how long to wait for the CDN")
	verify.Flags().DurationVar(&every, "every", 30*time.Second, "retry interval")

	var ddir, dver string
	drv := &cobra.Command{
		Use:   "playwright-driver",
		Short: "Assemble the Playwright driver from npm (playwright-core), integrity-checked",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return pwdriver.Install(cmd.Context(), ddir, dver)
		},
	}
	drv.Flags().StringVar(&ddir, "dir", "", "driver directory (PLAYWRIGHT_DRIVER_PATH)")
	drv.Flags().StringVar(&dver, "version", "1.57.0", "playwright-core version (must match playwright-go)")
	_ = drv.MarkFlagRequired("dir")

	root.AddCommand(build, serve, verify, drv)
	return root
}
