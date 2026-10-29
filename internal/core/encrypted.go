package core

import (
	"errors"
	"fmt"
	"io"

	"github.com/go-fde/luks"
)

// Encrypted images: a LUKS container, whose payload is then opened like any
// image -- its filesystem detected, or the one chosen. The password comes
// from the page for one open and is cleared once the container is unlocked
// or refused.

// EncLUKS is a LUKS1 or LUKS2 container, as Info.Encryption reports it.
const EncLUKS = "luks"

// ErrPassword is an encrypted image opened without a password.
var ErrPassword = errors.New("this image is encrypted: enter its password")

// luksLimits bound the key derivation a LUKS header may ask for. The header
// is untrusted and the browser derives the key on one thread, in a
// WebAssembly memory of at most 4 GiB: 2^22 PBKDF2 iterations, or 16 Argon2
// passes over 1 GiB (what cryptsetup's benchmark picks on a desktop), take
// seconds to tens of seconds.
var luksLimits = luks.Limits{
	MaxPBKDF2Iterations: 1 << 22,
	MaxArgon2Time:       16,
	MaxArgon2MemoryKiB:  1 << 20,
}

const luksMagic = "LUKS\xba\xbe" // offset 0, LUKS1 and LUKS2

// unlocked is the image the filesystem lives in.
type unlocked struct {
	r    io.ReaderAt
	size int64
	enc  string // EncLUKS, or empty
}

// unlock returns the image the filesystem lives in: r itself, or the
// decrypted payload of a LUKS container.
func unlock(r io.ReaderAt, size int64, password []byte) (unlocked, error) {
	var head [len(luksMagic)]byte
	if n, _ := r.ReadAt(head[:], 0); n < len(head) || string(head[:]) != luksMagic {
		return unlocked{r: r, size: size}, nil
	}
	if len(password) == 0 {
		return unlocked{enc: EncLUKS}, ErrPassword
	}
	dev, err := openLUKS(r, size, password)
	if err != nil {
		return unlocked{enc: EncLUKS}, err
	}
	off, n := dev.PayloadOffset(), dev.Size()
	if n == 0 { // the payload runs to the end of the image
		n = size - off
	}
	if off < 0 || n <= 0 || off > size || n > size-off {
		return unlocked{enc: EncLUKS}, fmt.Errorf("the LUKS payload (%d bytes at %d) lies outside the %d-byte image", n, off, size)
	}
	return unlocked{r: dev, size: n, enc: EncLUKS}, nil
}

// openLUKS unlocks the container with a panic of the header parser or the
// key derivation turned into an error, as guarded does for the drivers.
func openLUKS(r io.ReaderAt, size int64, password []byte) (dev *luks.Device, err error) {
	defer func() {
		if p := recover(); p != nil {
			dev, err = nil, fmt.Errorf("unlocking failed on this image: %v", p)
		}
	}()
	return luks.OpenFromLimited(rwDevice{roDevice{r: r, size: size}}, password, luksLimits)
}
