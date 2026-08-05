package record

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrInvalidRecording means the spool file failed validation (empty file or
// no moov box): recordVideo exits 0 with a 0-byte file when the target sim
// was not Booted, so success must be checked, never assumed.
var ErrInvalidRecording = errors.New("record: invalid recording")

// ValidateMP4 checks that the file is non-empty and its top-level box walk
// finds a moov box (the container was finalized).
func ValidateMP4(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return fmt.Errorf("%w: 0-byte file (was the simulator Booted?)", ErrInvalidRecording)
	}
	ok, err := hasMoov(f, fi.Size())
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidRecording, err)
	}
	if !ok {
		return fmt.Errorf("%w: no moov box (container not finalized)", ErrInvalidRecording)
	}
	return nil
}

// hasMoov walks the top-level ISO BMFF box headers looking for "moov".
func hasMoov(r io.ReaderAt, size int64) (bool, error) {
	var off int64
	var hdr [16]byte
	for off+8 <= size {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return false, err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr[:4]))
		boxType := string(hdr[4:8])
		headerLen := int64(8)
		switch boxSize {
		case 0: // box extends to end of file
			boxSize = size - off
		case 1: // 64-bit largesize
			if off+16 > size {
				return false, fmt.Errorf("truncated largesize box at offset %d", off)
			}
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return false, err
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:16]))
			headerLen = 16
		}
		if boxType == "moov" {
			return true, nil
		}
		if boxSize < headerLen || off+boxSize > size {
			return false, fmt.Errorf("malformed box %q (size %d) at offset %d", boxType, boxSize, off)
		}
		off += boxSize
	}
	return false, nil
}
