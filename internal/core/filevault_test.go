package core

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	apfs "github.com/go-filesystems/apfs"
)

// fileVaultImage is a container FormatContainerEncrypted writes: a keybag a
// passphrase unlocks, over a volume nothing has encrypted yet.
func fileVaultImage(t *testing.T, gpt bool) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fv.img")
	if err := os.WriteFile(path, nil, 0o600); err != nil { // Format wants it to exist
		t.Fatal(err)
	}
	format := apfs.FormatContainerEncrypted
	if gpt {
		format = apfs.FormatContainerEncryptedGPT
	}
	if err := format(path, 32<<20, "Vault", []byte("s3cret")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFileVault(t *testing.T) {
	for _, gpt := range []bool{false, true} {
		img := fileVaultImage(t, gpt)
		if !fileVaultCandidate(bytes.NewReader(img), int64(len(img))) {
			t.Fatalf("gpt %v: not a candidate", gpt)
		}
		s := NewSession(DefaultLimits)
		pass := []byte("s3cret")
		info, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", pass)
		if err != nil || info.Encryption != EncFileVault || info.Type != "apfs" {
			t.Fatalf("gpt %v: %+v, %v", gpt, info, err)
		}
		if !bytes.Equal(pass, make([]byte, len(pass))) {
			t.Errorf("gpt %v: the password was not cleared", gpt)
		}
		if _, err := s.List("/"); err != nil {
			t.Fatalf("gpt %v: List: %v", gpt, err)
		}
		info, err = s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", []byte("wrong"))
		if !errors.Is(err, apfs.ErrWrongPassphrase) || info.Encryption != EncFileVault {
			t.Fatalf("gpt %v: wrong password: %+v, %v", gpt, info, err)
		}
	}
}

// The fresh format encrypts nothing yet, so it also reads without the
// password: it opens plainly rather than asking for one.
func TestFileVaultReadableWithoutKey(t *testing.T) {
	img := fileVaultImage(t, false)
	s := NewSession(DefaultLimits)
	info, err := s.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil || info.Type != "apfs" || info.Encryption != "" {
		t.Fatalf("%+v, %v", info, err)
	}
}

// A candidate that a plain open cannot read asks for the password.
func TestFileVaultAsksForThePassword(t *testing.T) {
	img := fileVaultImage(t, false)
	clear(img[4096:]) // the superblock and its keylocker stay; nothing else reads
	s := NewSession(DefaultLimits)
	info, err := s.Open(bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, ErrPassword) || info.Encryption != EncFileVault {
		t.Fatalf("%+v, %v; want ErrPassword for filevault", info, err)
	}
}

func TestFileVaultCandidateRefuses(t *testing.T) {
	sample, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	if fileVaultCandidate(bytes.NewReader(sample), int64(len(sample))) {
		t.Error("the FAT32 sample is a candidate")
	}
	// A GPT header whose fields point anywhere must not be followed.
	junk := make([]byte, 64<<10)
	copy(junk[512:], "EFI PART")
	for _, f := range []func(){
		func() { junk[512+72] = 0xff; junk[512+79] = 0xff },                         // entries far past the end
		func() { clear(junk[512+72 : 512+80]); junk[512+72] = 2; junk[512+84] = 1 }, // entry size 1
	} {
		f()
		if fileVaultCandidate(bytes.NewReader(junk), int64(len(junk))) {
			t.Error("a junk GPT is a candidate")
		}
	}
}
