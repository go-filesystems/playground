package core

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-fde/luks"
)

// closableImage is a memImage a LUKS device can be formatted on.
type closableImage struct{ *memImage }

func (closableImage) Close() error { return nil }

// luksSample is the sample volume inside a LUKS1 container.
func luksSample(t *testing.T, password string) []byte {
	t.Helper()
	sample, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	const header = 132096 // luks.Format's payload offset, sector 258
	img := &memImage{b: make([]byte, header+len(sample))}
	dev, err := luks.FormatOn(closableImage{img}, []byte(password))
	if err != nil {
		t.Fatal(err)
	}
	if dev.PayloadOffset() != header {
		t.Fatalf("payload at %d, the test assumes %d", dev.PayloadOffset(), header)
	}
	if _, err := dev.WriteAt(sample, 0); err != nil {
		t.Fatal(err)
	}
	return img.b
}

func TestOpenLUKS(t *testing.T) {
	img := luksSample(t, "correct horse")
	s := NewSession(DefaultLimits)

	info, err := s.Open(bytes.NewReader(img), int64(len(img)))
	if !errors.Is(err, ErrPassword) || info.Encryption != EncLUKS {
		t.Fatalf("no password: %+v, %v; want ErrPassword for luks", info, err)
	}
	if _, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", []byte("wrong")); err == nil || errors.Is(err, ErrPassword) {
		t.Fatalf("wrong password: %v", err)
	}

	pass := []byte("correct horse")
	info, err = s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", pass)
	if err != nil {
		t.Fatal(err)
	}
	if info.Encryption != EncLUKS || info.Type != "fat32" {
		t.Fatalf("info = %+v, want fat32 inside luks", info)
	}
	if !bytes.Equal(pass, make([]byte, len(pass))) {
		t.Errorf("the password was not cleared: %q", pass)
	}
	l, err := s.List("/")
	if err != nil || len(l.Entries) == 0 {
		t.Fatalf("List = %+v, %v", l, err)
	}

	// The kind chosen applies to the filesystem inside.
	if info, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "fat32", []byte("correct horse")); err != nil || info.Type != "fat32" || !info.Forced {
		t.Fatalf("forced fat32 inside luks: %+v, %v", info, err)
	}
}

// A header that asks for more than the browser can derive is refused fast.
func TestOpenLUKSOverLimit(t *testing.T) {
	img := luksSample(t, "pw")
	// LUKS1 phdr: the key slots start at 208; slot 0's iterations are the
	// big-endian uint32 after its 4-byte active flag.
	binary.BigEndian.PutUint32(img[212:216], 1<<30)
	s := NewSession(DefaultLimits)
	_, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", []byte("pw"))
	if !errors.Is(err, luks.ErrLimit) {
		t.Fatalf("err = %v, want luks.ErrLimit", err)
	}
}

// A password given for an image that is not encrypted is ignored.
func TestPasswordForPlainImage(t *testing.T) {
	img, err := Sample()
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(DefaultLimits)
	info, err := s.OpenWith(bytes.NewReader(img), int64(len(img)), "auto", []byte("unused"))
	if err != nil || info.Encryption != "" || info.Type != "fat32" {
		t.Fatalf("%+v, %v", info, err)
	}
}
