//go:build js && wasm

// Command worker is the playground's engine, compiled to WebAssembly and run
// inside a dedicated Web Worker (web/worker.js). It reads the dropped File
// with FileReaderSync -- synchronous, and allowed only in workers (W3C File
// API, FileReaderSync is [Exposed=(DedicatedWorker,SharedWorker)]) -- so the
// drivers' synchronous io.ReaderAt works without blocking the page.
//
// It exposes globalThis.fsplay:
//
//	open(file: File, cache: boolean, kind: string, password?: string) -> string
//	    (JSON Response; kind is one of core.Kinds; password unlocks a LUKS image)
//	request(json: string)            -> [string, Uint8Array|null]
package main

import (
	"fmt"
	"io"
	"syscall/js"
	"time"

	"github.com/go-filesystems/playground/internal/core"
)

// fileReaderAt reads a browser File (a Blob) at an offset: Blob.slice gives
// the byte range, FileReaderSync.readAsArrayBuffer reads it synchronously.
type fileReaderAt struct {
	file js.Value
	frs  js.Value
	u8   js.Value
	size int64
}

func (f *fileReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, core.ErrNegativeOffset
	}
	if off >= f.size {
		return 0, io.EOF
	}
	end := min(off+int64(len(p)), f.size)
	defer func() {
		// readAsArrayBuffer throws NotReadableError / NotFoundError when
		// the file changed or went away; syscall/js turns that into a
		// panic, which here becomes the read's error.
		if r := recover(); r != nil {
			n, err = 0, fmt.Errorf("reading the image failed: %v", r)
		}
	}()
	ab := f.frs.Call("readAsArrayBuffer", f.file.Call("slice", float64(off), float64(end)))
	n = js.CopyBytesToGo(p, f.u8.New(ab))
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func main() {
	s := core.NewSession(core.DefaultLimits)
	u8 := js.Global().Get("Uint8Array")
	frsCtor := js.Global().Get("FileReaderSync")

	open := js.FuncOf(func(this js.Value, args []js.Value) any {
		t0 := time.Now()
		if len(args) < 1 || frsCtor.IsUndefined() {
			return string(core.OpenResponse(core.Info{}, fmt.Errorf("open needs a File inside a worker"), 0))
		}
		if len(args) > 1 {
			s.SetCache(args[1].Truthy())
		}
		kind := "auto"
		if len(args) > 2 && args[2].Type() == js.TypeString {
			kind = args[2].String()
		}
		var password []byte // cleared by OpenWith
		if len(args) > 3 && args[3].Type() == js.TypeString {
			password = []byte(args[3].String())
		}
		file := args[0]
		size := int64(file.Get("size").Float())
		r := &fileReaderAt{file: file, frs: frsCtor.New(), u8: u8, size: size}
		info, err := s.OpenWith(r, size, kind, password)
		return string(core.OpenResponse(info, err, float64(time.Since(t0).Microseconds())/1000))
	})
	request := js.FuncOf(func(this js.Value, args []js.Value) any {
		if len(args) < 1 || args[0].Type() != js.TypeString {
			resp, _ := s.Handle(nil)
			return []any{string(resp), nil}
		}
		resp, data := s.Handle([]byte(args[0].String()))
		var out any
		if data != nil {
			a := u8.New(len(data))
			js.CopyBytesToJS(a, data)
			out = a
		}
		return []any{string(resp), out}
	})
	js.Global().Set("fsplay", js.ValueOf(map[string]any{"open": open, "request": request}))
	select {}
}
