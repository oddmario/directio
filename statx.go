//go:build linux
// +build linux

package directio

import (
	"errors"

	"golang.org/x/sys/unix"
)

var ErrFSNoDIOSupport = errors.New("filesystem does not expose Direct I/O alignment")

func DIOMemAlign(path string) (uint32, error) {
	var stx unix.Statx_t

	// Ask statx for direct I/O info. On Linux ≥6.1, STATX_DIOALIGN returns
	// stx.Dio_mem_align and stx.Dio_offset_align.
	mask := unix.STATX_DIOALIGN

	flags := unix.AT_STATX_SYNC_AS_STAT | unix.AT_NO_AUTOMOUNT
	if err := unix.Statx(unix.AT_FDCWD, path, flags, mask, &stx); err != nil {
		switch {
		case errors.Is(err, unix.ENOSYS),
			errors.Is(err, unix.EOPNOTSUPP),
			errors.Is(err, unix.ENOTSUP):
			return 0, ErrFSNoDIOSupport
		}
		return 0, err
	}

	// Check which bits were actually filled by the kernel/FS.
	if (stx.Mask & unix.STATX_DIOALIGN) == 0 {
		return 0, ErrFSNoDIOSupport
	}

	if stx.Dio_mem_align == 0 {
		return 0, ErrFSNoDIOSupport
	}

	return stx.Dio_mem_align, nil
}
