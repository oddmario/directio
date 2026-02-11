package directio

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	// Default buffer is 16KB (4 pages).
	defaultBufSize = 16384
)

var _ io.WriteCloser = (*DirectIO)(nil)

// align returns an offset for alignment for buffer b and size.
func align(b []byte, size int) int {
	if size <= 0 || len(b) == 0 {
		return 0
	}

	return int(uintptr(unsafe.Pointer(&b[0])) % uintptr(size))
}

// allocAlignedBuf allocates buffer of size n that is aligned by blockSize.
func allocAlignedBuf(blockSize, n int) ([]byte, error) {
	if blockSize <= 0 {
		return nil, errors.New("invalid block size")
	}
	if n <= 0 {
		return nil, errors.New("size must be greater than zero")
	}

	// Allocate memory buffer
	buf := make([]byte, n+blockSize)

	// First memmory alignment
	a1 := align(buf, blockSize)
	offset := 0
	if a1 != 0 {
		offset = blockSize - a1
	}

	buf = buf[offset : offset+n]

	// Was alredy aligned. So just exit
	if a1 == 0 {
		return buf, nil
	}

	// Second alignment – check and exit
	a2 := align(buf, blockSize)
	if a2 != 0 {
		return nil, errors.New("can't allocate aligned buffer")
	}

	return buf, nil
}

// DirectIO bypasses page cache.
type DirectIO struct {
	f         *os.File
	buf       []byte
	n         int
	err       error
	blockSize int
	isClosed  bool
}

func GetBestAlignment(path string) int {
	var stat syscall.Statfs_t

	// Ensure we check the directory if the file doesn't exist yet
	checkPath := path
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		checkPath = filepath.Dir(path)
	}

	if err := syscall.Statfs(checkPath, &stat); err != nil {
		// Fallback: 4KB is the safest bet for almost all modern Linux servers
		return 4096
	}

	// Usually 4096 on ext4/xfs/btrfs
	blockSize := int(stat.Bsize)

	// O_DIRECT usually requires at least 512.
	// If Statfs returns something weird (like 0 or 1), force 4096.
	if blockSize < 512 {
		return 4096
	}

	// Optimization: If the FS says 512, but we are on a 4Kn drive,
	// 512 writes will be slow (Read-Modify-Write).
	// It is almost always better to upgrade 512 -> 4096.
	if blockSize < 4096 {
		return 4096
	}

	return blockSize
}

// NewSize returns a new DirectIO writer.
func NewSize(f *os.File, size int) (*DirectIO, error) {
	if err := checkDirectIO(f.Fd()); err != nil {
		return nil, err
	}

	// Get the file optimal block size dynamically
	align := GetBestAlignment(f.Name())

	blockSize := align

	if size <= 0 {
		size = defaultBufSize
	}
	if size < defaultBufSize {
		size = defaultBufSize
	}
	if rem := size % blockSize; rem != 0 {
		size += blockSize - rem
	}

	buf, err := allocAlignedBuf(blockSize, size)
	if err != nil {
		return nil, err
	}

	return &DirectIO{
		buf:       buf,
		f:         f,
		blockSize: blockSize,
		isClosed:  false,
	}, nil
}

// New returns a new DirectIO writer with default buffer size.
func New(f *os.File) (*DirectIO, error) {
	return NewSize(f, defaultBufSize)
}

// flush writes buffered data to the underlying os.File.
func (d *DirectIO) flush() error {
	if d.err != nil {
		return d.err
	}

	if d.n == 0 {
		return nil
	}

	n, err := d.f.Write(d.buf[0:d.n])

	if n < d.n && err == nil {
		err = io.ErrShortWrite
	}

	if err != nil {
		if n > 0 && n < d.n {
			copy(d.buf[0:d.n-n], d.buf[n:d.n])
		}
	}

	d.n -= n
	return err
}

// Available returns how many bytes are unused in the buffer.
func (d *DirectIO) Available() int { return len(d.buf) - d.n }

// Buffered returns the number of bytes that have been written into the current buffer.
func (d *DirectIO) Buffered() int { return d.n }

// Write writes the contents of p into the buffer.
// It returns the number of bytes written.
// If nn < len(p), it also returns an error explaining
// why the write is short.
func (d *DirectIO) Write(p []byte) (nn int, err error) {
	if d.isClosed {
		return 0, errors.New("the writer is closed")
	}

	// Write more than available in buffer.
	for len(p) >= d.Available() && d.err == nil {
		var n int
		// Check if buffer is zero size for direct and zero copy write to Writer.
		// Here we also check the p memory alignment.
		// If buffer p is not aligned, than write through buffer d.buf and flush.
		if d.Buffered() == 0 && align(p, d.blockSize) == 0 {
			// Large write, empty buffer.
			if (len(p) % d.blockSize) == 0 {
				// Data and buffer p are already aligned to block size.
				// So write directly from p to avoid copy.
				n, d.err = d.f.Write(p)
			} else {
				// Data needs alignment. Buffer alredy aligned.

				// Align data
				l := len(p) & -d.blockSize

				// Write directly from p to avoid copy.
				var nl int
				nl, d.err = d.f.Write(p[:l])

				// Save other data to buffer.
				n = copy(d.buf[d.n:], p[l:])
				d.n += n

				// written and buffered data
				n += nl
			}
		} else {
			n = copy(d.buf[d.n:], p)
			d.n += n
			err = d.flush()
			if err != nil {
				return nn, err
			}
		}
		nn += n
		p = p[n:]
	}

	if d.err != nil {
		return nn, d.err
	}

	n := copy(d.buf[d.n:], p)
	d.n += n
	nn += n

	return nn, nil
}

// Close writes any data left in the writer buffer
//
// Note that this function doesn't close the underlying os.File
// it's the caller's responsibility to close the underlying os.File
//
// If the last bit of data aren't in a perfect aligned block, Close also calls Sync() on the underlying os.File
func (d *DirectIO) Close() error {
	if d.isClosed {
		return errors.New("the writer is already closed")
	}

	d.isClosed = true

	if d.n == 0 {
		return nil
	}

	// 1. Calculate the bulk size that is safe for O_DIRECT
	//    (Must be a multiple of blockSize)
	alignedSize := d.n - (d.n % d.blockSize)

	// 2. Phase 1: Write the Aligned Bulk (Direct I/O)
	//    We do this first while O_DIRECT is still enabled.
	if alignedSize > 0 {
		n, err := d.f.Write(d.buf[:alignedSize])
		if err != nil {
			return err
		}

		// Shift the remaining "tail" data to the start of the buffer
		copy(d.buf, d.buf[n:d.n])
		d.n -= n
	}

	// 3. Phase 2: Write the Tail (Buffered I/O)
	//    If there are any bytes left (the unaligned remainder),
	//    we must disable O_DIRECT to write them safely.
	if d.n > 0 {
		// Disable Direct IO temporarily
		if err := setDirectIO(d.f.Fd(), false); err != nil {
			return err
		}

		// Standard buffered write (touches Page Cache)
		n, err := d.f.Write(d.buf[:d.n])

		// CRITICAL: Re-enable Direct IO immediately
		// Even if the write failed, we try to restore the state.
		_ = setDirectIO(d.f.Fd(), true)

		if err != nil {
			return err
		}
		d.n -= n

		d.f.Sync() // sync the file to flush the final bit of data to the disk immediately

		// Advise the kernel to drop the pagecache immediately for the data that we wrote without O_DIRECT above
		// Fd() returns uintptr, Fadvise expects int
		fd := int(d.f.Fd())

		// Arguments: File Descriptor, Offset (0), Length (0 = all), Advice
		unix.Fadvise(fd, 0, 0, unix.FADV_DONTNEED)
	}

	return nil
}
