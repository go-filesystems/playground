//go:build !(js && wasm)

// Command worker is the playground's WebAssembly engine; it only runs in a
// browser Web Worker. Build it with GOOS=js GOARCH=wasm (see cmd/site).
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "worker: build with GOOS=js GOARCH=wasm and run it in a browser (go run ./cmd/site serve)")
	os.Exit(2)
}
