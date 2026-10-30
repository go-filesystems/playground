package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	apfsfde "github.com/go-fde/apfs"
	apfs "github.com/go-filesystems/apfs"
	"github.com/go-filesystems/detect"
	filesystem "github.com/go-filesystems/interface"
)

// APFS FileVault: an APFS container whose volume is encrypted with a key a
// passphrase unlocks (go-filesystems/apfs v0.4.0, keys from go-fde/apfs). The
// container's metadata is plaintext, so a plain open may get some way before
// the encrypted file-system tree stops it.

// EncFileVault is an APFS FileVault volume, as Info.Encryption reports it.
const EncFileVault = "filevault"

// fileVaultLimits bound the key derivation a keybag may ask for, as
// luksLimits do for a LUKS header.
var fileVaultLimits = apfsfde.Limits{
	MaxPBKDF2Iterations: 1 << 22,
	MaxArgon2Time:       16,
	MaxArgon2MemoryKiB:  1 << 20,
}

const (
	nxMagicOffset     = 32
	nxKeylockerOffset = 1296 // nx_keylocker, a prange: paddr then block count
	gptHeaderLBA      = 512
)

// apfsPartitionType is the APFS partition type GUID,
// 7C3457EF-0000-11AA-AA11-00306543ECAC, as GPT stores it (the first three
// fields little-endian).
var apfsPartitionType = []byte{0xEF, 0x57, 0x34, 0x7C, 0x00, 0x00, 0xAA, 0x11, 0xAA, 0x11, 0x00, 0x30, 0x65, 0x43, 0xEC, 0xAC}

// fileVaultCandidate says whether r holds an APFS container that has a
// keybag (nx_keylocker set): at offset 0, or at the first APFS partition of
// a GPT with 512-byte sectors. Only such a container can be FileVault.
func fileVaultCandidate(r io.ReaderAt, size int64) bool {
	if hasKeybag(r, 0) {
		return true
	}
	var hdr [92]byte
	if n, _ := r.ReadAt(hdr[:], gptHeaderLBA); n < len(hdr) || string(hdr[:8]) != "EFI PART" {
		return false
	}
	entries := int64(binary.LittleEndian.Uint64(hdr[72:80]))
	count := min(int64(binary.LittleEndian.Uint32(hdr[80:84])), 128)
	esize := int64(binary.LittleEndian.Uint32(hdr[84:88]))
	if esize < 128 || esize > 4096 || entries <= 0 || entries > size/512 {
		return false
	}
	e := make([]byte, 48)
	for i := int64(0); i < count; i++ {
		if n, _ := r.ReadAt(e, entries*512+i*esize); n < len(e) {
			return false
		}
		if !bytes.Equal(e[:16], apfsPartitionType) {
			continue
		}
		first := binary.LittleEndian.Uint64(e[32:40])
		if first == 0 || first > uint64(size/512) {
			return false
		}
		return hasKeybag(r, int64(first)*512)
	}
	return false
}

// hasKeybag says whether an APFS container superblock at off names a keybag.
func hasKeybag(r io.ReaderAt, off int64) bool {
	var b [nxKeylockerOffset + 8]byte
	if n, _ := r.ReadAt(b[:], off); n < len(b) || string(b[nxMagicOffset:nxMagicOffset+4]) != "NXSB" {
		return false
	}
	return binary.LittleEndian.Uint64(b[nxKeylockerOffset:]) != 0
}

// openMaybeFileVault opens r as kind, trying FileVault first when it may be
// one. With a password, a FileVault volume opens through its key, and an
// image that turns out not to be one opens plainly. Without one, a candidate
// that a plain open cannot list asks for the password (ErrPassword). maybe
// is false when the image was a LUKS payload: no FileVault inside that.
func openMaybeFileVault(r io.ReaderAt, size int64, kind string, password []byte, maybe bool) (filesystem.Filesystem, detect.Type, string, error) {
	if !maybe || (kind != "auto" && kind != "apfs") || !fileVaultCandidate(r, size) {
		fsys, t, err := openGuarded(r, size, kind)
		return fsys, t, "", err
	}
	if len(password) > 0 {
		fsys, _, err := guarded(func() (filesystem.Filesystem, detect.Type, error) {
			fsys, err := apfs.OpenFDEFromBlockDeviceLimited(roDevice{r: r, size: size}, password, -1, fileVaultLimits)
			return fsys, "apfs", err
		})
		switch {
		case err == nil:
			return fsys, "apfs", EncFileVault, nil
		case errors.Is(err, apfs.ErrWrongPassphrase), errors.Is(err, apfs.ErrLimit):
			return nil, "apfs", EncFileVault, err
		}
		// Not FileVault after all: the plain open says what it is.
	}
	fsys, t, err := openGuarded(r, size, kind)
	if err == nil {
		if _, lerr := guardedList(fsys); lerr == nil {
			return fsys, t, "", nil
		}
		fsys.Close()
		err = errors.New("the volume cannot be read without its key")
	}
	if len(password) == 0 {
		return nil, "apfs", EncFileVault, fmt.Errorf("%w (%v)", ErrPassword, err)
	}
	return nil, t, "", err
}

// guardedList lists the root with a driver panic turned into an error.
func guardedList(fsys filesystem.Filesystem) (n int, err error) {
	defer guard(&err)
	ents, err := fsys.ListDir("/")
	return len(ents), err
}
