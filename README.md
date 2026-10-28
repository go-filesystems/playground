# go-filesystems playground

[![ci](https://github.com/go-filesystems/playground/actions/workflows/ci.yml/badge.svg)](https://github.com/go-filesystems/playground/actions/workflows/ci.yml)
[![pages](https://github.com/go-filesystems/playground/actions/workflows/pages.yml/badge.svg)](https://github.com/go-filesystems/playground/actions/workflows/pages.yml)

**Try it: https://go-filesystems.github.io/playground/**

Drop a disk image into a web page. The page says which filesystem it is, lets you
browse the tree, view small files and download any file. Everything runs in your
browser, in WebAssembly: **nothing is uploaded**.

No image at hand? **Make a sample FAT32 image**: the page formats one in memory
with [`fat32.FormatImage`](https://github.com/go-filesystems/fat32), writes a few
files into it and opens it again; you can download it too.

## What it opens

A filesystem at offset 0 of the file (a filesystem image, not a partitioned disk),
detected by [`go-filesystems/detect`](https://github.com/go-filesystems/detect) and
opened by the same eight pure-Go drivers
[go-fileshare](https://github.com/go-fileshare/fileshare) serves:

| Filesystem | Driver | Partial reads |
|---|---|---|
| FAT32 | [fat32](https://github.com/go-filesystems/fat32) | yes |
| exFAT | [exfat](https://github.com/go-filesystems/exfat) | yes |
| ext4 | [ext4](https://github.com/go-filesystems/ext4) | yes |
| ISO 9660 | [iso9660](https://github.com/go-filesystems/iso9660) | yes |
| SquashFS | [squashfs](https://github.com/go-filesystems/squashfs) | yes |
| NTFS | [ntfs](https://github.com/go-filesystems/ntfs) | whole file, up to 64 MiB |
| HFS+ | [hfsplus](https://github.com/go-filesystems/hfsplus) | whole file, up to 64 MiB |
| UFS | [ufs](https://github.com/go-filesystems/ufs) | whole file, up to 64 MiB |

"Partial reads" means the driver implements `filesystem.Opener`, so viewing or
downloading streams the file in 4 MiB pieces; the others can only read a file
whole, which the playground bounds. apfs, btrfs, xfs and zfs open a partition of
a disk image by path, a shape `detect.Register` does not take, so they are not
here yet.

## How it works

- The Go engine (`cmd/worker`, `GOOS=js GOARCH=wasm`) runs in a **Web Worker**.
  Its `io.ReaderAt` reads the dropped `File` with
  `FileReaderSync.readAsArrayBuffer(file.slice(off, end))`
  ([W3C File API](https://w3c.github.io/FileAPI/): synchronous, and exposed only in
  workers), so the drivers' synchronous reads work without blocking the page and
  without ever loading the whole image.
- A **block cache** (64 KiB blocks, LRU, 16 MiB) sits between the drivers and the
  file: a driver's many small reads (a sector, a directory entry, an inode) become
  a few block reads. Large reads (file contents) bypass it. The checkbox on the
  page turns it off, to compare.
- The page is plain HTML, CSS and a few KiB of JavaScript. It talks to the worker
  with three coarse requests: open, list a directory, read part of a file.

## Security

The image is untrusted input.

- Every answer is bounded: 10 000 entries per listing, 4 MiB per read, 64 MiB for a
  driver that can only read a file whole. A file is never loaded whole to be
  streamed.
- A driver panic on a damaged image becomes an error; every request has a 30 s
  timeout, after which the worker is terminated and restarted, so a corrupt image
  cannot hang the page.
- Names from the image reach the DOM through `textContent` only (an end-to-end
  test opens an ext4 image whose file names are HTML); download names are
  sanitised.
- The page's Content-Security-Policy allows only its own scripts and WebAssembly.
- Requests to the engine are decoded strictly and fuzzed (`FuzzHandle`), and so is
  browsing a damaged image (`FuzzImage`).

## Develop

```sh
go test ./...                                   # engine, site tool
go run ./cmd/site build --out _site             # the site: page + fsplay.wasm + wasm_exec.js
go run ./cmd/site serve --dir _site             # http://127.0.0.1:8080/playground/

# headless Chromium end to end
go run ./cmd/site playwright-driver --dir /tmp/pwdriver
PLAYWRIGHT_DRIVER_PATH=/tmp/pwdriver PLAYWRIGHT_NODEJS_PATH=$(command -v node) \
  go test -tags e2e ./e2e/
```

`E2E_IMAGE=/path/to/image` makes `TestMeasureCache` measure open and root-listing
times on that image with and without the block cache.

## License

See [LICENSE](LICENSE).
