// Package healthcheck implements a read-only, streaming media inventory and
// synchronization probe for WAV/Y4M pairs on local disk.
//
// Every scanner in this package reads files through boundedReader so that a
// batch whose total size exceeds the process memory budget can still be
// processed: the working set per file is capped by the configured budget
// instead of growing with the file size.
package healthcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
)

// minBudgetBytes is the smallest read buffer a scan will use. It must hold a
// Y4M frame marker line and a few WAV chunk headers.
const minBudgetBytes = 64 * 1024

// maxBudgetBytes caps the default block size even when a large budget is
// requested; larger blocks buy little beyond this point.
const maxBudgetBytes = 4 * 1024 * 1024

// boundedReader streams a file through one reusable fixed-size block while
// maintaining a running SHA-256 digest. It never holds more than one block
// plus the bytes explicitly retained by the caller, giving the scan a hard
// working-set ceiling independent of file size.
type boundedReader struct {
	file   *os.File
	hash   hash.Hash
	block  []byte
	start  int // valid bytes start at block[start]
	length int // number of valid bytes in block
	offset int64
	closed bool
}

func newBoundedReader(file *os.File, budget int) *boundedReader {
	if budget < minBudgetBytes {
		budget = minBudgetBytes
	}
	if budget > maxBudgetBytes {
		budget = maxBudgetBytes
	}
	return &boundedReader{
		file:  file,
		hash:  sha256.New(),
		block: make([]byte, budget),
	}
}

// refill ensures at least one byte is available in the block. It returns
// false at clean end of file.
func (r *boundedReader) refill() (bool, error) {
	if r.start < r.length {
		return true, nil
	}
	n, err := r.file.Read(r.block)
	r.length = n
	r.start = 0
	if n > 0 {
		r.hash.Write(r.block[:n])
		r.offset += int64(n)
	}
	if err == io.EOF {
		return n > 0, nil
	}
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// readByte consumes one byte.
func (r *boundedReader) readByte() (byte, bool, error) {
	ok, err := r.refill()
	if err != nil || !ok {
		return 0, false, err
	}
	value := r.block[r.start]
	r.start++
	return value, true, nil
}

// readExact fills dst entirely from the stream using copy so dst never grows
// beyond its declared capacity.
func (r *boundedReader) readExact(dst []byte) error {
	for len(dst) > 0 {
		ok, err := r.refill()
		if err != nil {
			return err
		}
		if !ok {
			return io.ErrUnexpectedEOF
		}
		available := r.block[r.start:r.length]
		taken := copy(dst, available)
		r.start += taken
		dst = dst[taken:]
	}
	return nil
}

// discard skips count bytes while still feeding the digest.
func (r *boundedReader) discard(count int64) error {
	for count > 0 {
		ok, err := r.refill()
		if err != nil {
			return err
		}
		if !ok {
			return io.ErrUnexpectedEOF
		}
		available := int64(r.length - r.start)
		if available > count {
			available = count
		}
		r.start += int(available)
		count -= available
	}
	return nil
}

// readUntil consumes bytes up to and including delimiter and invokes visit for
// each consumed byte. A non-nil visit error aborts the read.
func (r *boundedReader) readUntil(delimiter byte, visit func(byte) error) error {
	for {
		value, ok, err := r.readByte()
		if err != nil {
			return err
		}
		if !ok {
			return io.ErrUnexpectedEOF
		}
		if err := visit(value); err != nil {
			return err
		}
		if value == delimiter {
			return nil
		}
	}
}

// position reports how many payload bytes have been streamed so far.
func (r *boundedReader) position() int64 {
	return r.offset - int64(r.length-r.start)
}

func (r *boundedReader) digest() string {
	return hex.EncodeToString(r.hash.Sum(nil))
}

func (r *boundedReader) close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	return r.file.Close()
}

// openBounded opens path exclusively through os.Open (read-only) and returns a
// reader plus the stat captured at the open boundary.
func openBounded(path string, budget int) (*boundedReader, os.FileInfo, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	return newBoundedReader(file, budget), info, nil
}

// hashFile re-streams a file and returns only its SHA-256 digest. It is used
// for the optional content-unchanged verification pass and reuses the same
// budgeted block size as the main scan.
func hashFile(path string, budget int) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	reader := newBoundedReader(file, budget)
	for {
		ok, err := reader.refill()
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		reader.start = reader.length
	}
	return reader.digest(), nil
}

// errScan marks an expected media rejection. The driver treats it as a
// deterministic probe finding rather than an operational failure.
type scanError struct {
	code    string
	message string
}

func (e *scanError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %s", e.code, e.message)
}

func reject(code, format string, args ...any) error {
	return &scanError{code: code, message: fmt.Sprintf(format, args...)}
}

// asScanError extracts a deterministic probe rejection if present.
func asScanError(err error) *scanError {
	var target *scanError
	if errors.As(err, &target) {
		return target
	}
	return nil
}
