package core

import (
	"errors"
	"io"
	"os"

	apfs "github.com/go-filesystems/apfs"
	btrfs "github.com/go-filesystems/btrfs"
	"github.com/go-filesystems/detect"
	filesystem "github.com/go-filesystems/interface"
	xfs "github.com/go-filesystems/xfs"
	zfs "github.com/go-filesystems/zfs"
)

// The drivers that open a partition of a whole disk image through a block
// device rather than a filesystem at offset zero: detect cannot sniff them
// (there is no magic at offset zero of a disk), so the page names them.
// They take a read-write block device; the playground's is read-only, and a
// driver that tries to write gets os.ErrPermission.

// Kinds are the filesystem types a person can choose, "auto" first: auto
// is detect's answer, the rest force a driver.
var Kinds = []string{
	"auto",
	string(detect.FAT32), string(detect.ExFAT), string(detect.Ext4), string(detect.NTFS),
	string(detect.ISO9660), string(detect.SquashFS), string(detect.HFSPlus), string(detect.UFS),
	"apfs", "btrfs", "xfs", "zfs",
}

// ErrUnknownKind is a type the page asked for that is not one of Kinds.
var ErrUnknownKind = errors.New("unknown filesystem type")

// roDevice is a read-only block device over an io.ReaderAt of known size.
type roDevice struct {
	r    io.ReaderAt
	size int64
}

func (d roDevice) ReadAt(p []byte, off int64) (int, error) { return d.r.ReadAt(p, off) }
func (d roDevice) WriteAt([]byte, int64) (int, error)      { return 0, os.ErrPermission }
func (d roDevice) Sync() error                             { return nil }
func (d roDevice) Size() (int64, error)                    { return d.size, nil }
func (d roDevice) Truncate(int64) error                    { return os.ErrPermission }
func (d roDevice) Close() error                            { return nil }

// rwDevice is a block device over r that writes when r can: the COW, under a
// LUKS container, so that a change is encrypted into the image.
type rwDevice struct{ roDevice }

func (d rwDevice) WriteAt(p []byte, off int64) (int, error) {
	if w, ok := d.r.(io.WriterAt); ok {
		return w.WriteAt(p, off)
	}
	return 0, os.ErrPermission
}

// openDevice opens r with the device-shaped driver kind, the partition found
// in its table (or the whole image).
func openDevice(kind string, r io.ReaderAt, size int64) (filesystem.Filesystem, error) {
	dev := roDevice{r: r, size: size}
	switch kind {
	case "apfs":
		return apfs.OpenFromBlockDevice(dev, -1)
	case "btrfs":
		return btrfs.OpenFromDevice(dev, -1)
	case "xfs":
		return xfs.OpenFromDevice(dev, -1)
	case "zfs":
		return zfs.OpenFromDevice(dev, -1)
	}
	return nil, ErrUnknownKind
}
