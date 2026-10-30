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
| APFS | [apfs](https://github.com/go-filesystems/apfs) | whole file, up to 64 MiB |
| Btrfs | [btrfs](https://github.com/go-filesystems/btrfs) | whole file, up to 64 MiB |
| XFS | [xfs](https://github.com/go-filesystems/xfs) | whole file, up to 64 MiB |
| ZFS | [zfs](https://github.com/go-filesystems/zfs) | whole file, up to 64 MiB |

"Partial reads" means the driver implements `filesystem.Opener`, so viewing or
downloading streams the file in 4 MiB pieces; the others can only read a file
whole, which the playground bounds.

**The Filesystem list** chooses the driver. *Detect it*, the default, asks
`detect`; when it finds nothing it can open at offset 0, the four disk-image
drivers -- APFS, Btrfs, XFS, ZFS, which take a block device and find their
partition in a GPT or MBR table themselves -- are tried in turn. Choosing a
filesystem forces that driver, for an image `detect` gets wrong or cannot
recognise. The block device they read is read-only: a driver that tries to
write gets a permission error. The four add about 1 MB to the compressed
engine (2.39 → 3.39 MB gzip).

**Encrypted images.** A LUKS1 or LUKS2 container
([go-fde/luks](https://github.com/go-fde/luks)) is recognised by its header, and
the page asks for its password. The engine unlocks it and opens the filesystem in
the payload: detected, or the one chosen in the list. Wrong passwords are refused,
and the form stays to try again. APFS FileVault containers are not supported yet:
go-filesystems/apfs cannot read one through its decrypted device so far, and that is
being fixed there first.

**Open in the explorer.** Once an image is open, *open in the explorer* loads the
[go-fileshare explorer](https://github.com/go-fileshare/explorer) (v0.4.0 or later)
in a same-origin iframe. It is fetched only then: 4.1 MB compressed. It browses the
image the way it browses a fileshare server, over WebDAV. Its requests do not go to
any network. The page hands them to the engine, which answers them with
[go-filesystems/webdav](https://github.com/go-filesystems/webdav) over the open
image. Closing the explorer unloads it.

**Changing the image.** In the explorer you can upload, make folders, rename,
copy and delete, on FAT32, exFAT and ext4: the drivers that write through
`OpenReader`. The other filesystems refuse writes. Nothing is written to the file
you dropped, which a page can only read. A change lands in a **copy-on-write**
layer under the drivers, in the tab's memory: at most 256 MiB, in 64 KiB blocks.
Inside a LUKS container, it lands encrypted. The page then shows how much changed,
and **download the modified image** saves `<name>-modified.<ext>`: the original
with the changes in it, in the same format. It is read back from the engine in
4 MiB pieces. Opening another image, or leaving the page, asks first while changes
are not downloaded.

FAT32 images made by `go-filesystems/fat32`, the sample among them, draw two
warnings from `fsck_msdos`: a `..` entry that should be 0 under the root, and an
FSInfo free count left unset. Both come from the driver and are being fixed there.
The copy-on-write layer adds none.

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
- The explorer's WebDAV goes through a **bridge**:
  1. the explorer finds `fileshareExplorerBridge` on its parent page;
  2. the page posts each request to the worker;
  3. `core.Session.DAV` runs it through the WebDAV handler.
  Every request and answer is bounded at 16 MiB. The WebDAV server adds 0.46 MB to
  the compressed engine (3.44 → 3.90 MB).

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
- **A password** goes from the field to the engine for one open. It is not stored
  and never leaves the tab. The page empties the field on submit, the worker passes
  it straight on, and the engine clears its byte copy once the container is
  unlocked or refused. JavaScript and Go strings cannot be wiped, though, so a copy
  may stay in the tab's memory until it is garbage-collected. Close the tab when
  done with a sensitive image.
- **The LUKS header is untrusted** too: it states how much key derivation unlocking
  costs. The engine refuses a header that asks for more than 2²² PBKDF2 iterations,
  or for Argon2 beyond 16 passes or 1 GiB (go-fde/luks `OpenFromLimited`), before
  deriving anything. Within those limits an unlock gets 180 s instead of 30.
- **Writes stay in the tab.** The dropped file is never written: changes go to an
  in-memory copy-on-write of the image, bounded at 256 MiB. A write that would pass
  that bound is refused whole. Nothing changed leaves the tab unless you download
  the modified image.
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
