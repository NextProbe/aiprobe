package sqlitedb

// Reading a write-ahead log
//
// # Why this is not optional
//
// A database in WAL mode keeps recently committed pages in a `-wal` sidecar
// until a checkpoint folds them back into the main file. Cursor holds its chat
// store open for as long as the editor is running, so on any machine where Cursor
// is installed and used the newest conversations live partly in that sidecar, and
// the global store is in WAL mode permanently. Reading the main file alone finds
// an empty `cursorDiskKV` and reports "no sessions" on a machine with hundreds of
// them.
//
// Refusing to read the main file was the earlier behaviour, and it was honest but
// useless: the refusal covered every real Cursor installation. So the log is read
// too.
//
// # What is implemented
//
// Only the read half of WAL recovery, which is the part that decides which frames
// in the file are committed content and which are leftovers from before the last
// checkpoint. That is: the 32-byte header, the 24-byte frame headers, the salt
// match, the cumulative Fibonacci-weighted checksum, and the commit markers that
// say where a transaction ended.
//
// There is no locking and no shared-memory index. The `-shm` file is ignored and
// the log is recovered by scanning it, which is what SQLite itself does when no
// other connection has already built the index. A concurrent writer can therefore
// append frames while this runs; the scan stops at the last frame whose checksum
// chain is intact, so a half-written frame is left out rather than read as
// garbage.
//
// # Why a checksum failure ends the scan rather than the read
//
// The checksums are the only thing distinguishing a live frame from a stale one
// that a previous WAL generation left in the file, so a mismatch is not corruption
// but the end of the valid region. Everything up to the last commit marker before
// it is a consistent snapshot of the database, which is exactly what a reader
// should see.

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// walSuffix is appended to a database path to name its log.
const walSuffix = "-wal"

const (
	walHeaderSize      = 32
	walFrameHeaderSize = 24

	// walMagicLE and walMagicBE differ only in which byte order the checksums
	// were computed in. The stored checksum fields are big-endian either way.
	walMagicLE = 0x377f0682
	walMagicBE = 0x377f0683

	// walFormat is the only log format version SQLite has ever written, and the
	// only one it will read.
	walFormat = 3007000
)

// walIndex is the committed content of a log: the newest version of every page a
// transaction wrote, and how large the database was when the last one committed.
type walIndex struct {
	// pages maps a 1-based page number to its content. The slices alias the log
	// bytes rather than copying, since the log is already in memory.
	pages map[int][]byte
	// dbSize is the database size in pages recorded by the last commit frame. It
	// is authoritative over the main file's header, which a WAL-mode writer does
	// not keep current.
	dbSize int
	// frames is how many valid frames were consumed, for diagnostics.
	frames int
}

// readWAL recovers the committed pages of a log.
//
// A nil index with a nil error means the log holds nothing that overrides the main
// file: it is absent, header-only, or contains no completed transaction. That is
// an ordinary state, not a failure, and it is what a freshly checkpointed database
// looks like.
//
// An error means the file is present but cannot be interpreted, which is reported
// rather than skipped: silently ignoring an unreadable log is the stale read this
// package exists to avoid.
func readWAL(log []byte, pageSize int) (*walIndex, error) {
	if len(log) == 0 {
		return nil, nil
	}
	if len(log) < walHeaderSize {
		return nil, fmt.Errorf("%w: the log is %d bytes, shorter than its 32-byte header", ErrWALMode, len(log))
	}

	var bigEndianSums bool
	switch magic := binary.BigEndian.Uint32(log[0:4]); magic {
	case walMagicBE:
		bigEndianSums = true
	case walMagicLE:
		bigEndianSums = false
	default:
		return nil, fmt.Errorf("%w: the log starts with %#08x, which is not a write-ahead log signature", ErrWALMode, magic)
	}

	if v := binary.BigEndian.Uint32(log[4:8]); v != walFormat {
		return nil, fmt.Errorf("%w: log format version %d is not the %d this reader understands", ErrWALMode, v, walFormat)
	}

	// A log written for a different page size belongs to a different database.
	// Continuing would frame the file at the wrong stride and checksum noise.
	if ps := int(binary.BigEndian.Uint32(log[8:12])); ps != pageSize {
		return nil, fmt.Errorf("%w: the log has %d-byte pages but the database has %d-byte pages", ErrWALMode, ps, pageSize)
	}

	salt := log[16:24]
	sum0, sum1 := walChecksum(0, 0, log[:24], bigEndianSums)
	if sum0 != binary.BigEndian.Uint32(log[24:28]) || sum1 != binary.BigEndian.Uint32(log[28:32]) {
		// The header checksum covers the salts and the page size. Without it there
		// is no way to tell a live frame from a leftover, so no frame can be
		// trusted and the main file may be stale.
		return nil, fmt.Errorf("%w: the log header checksum does not match, so its frames cannot be told apart from leftovers", ErrWALMode)
	}

	// Pages written by a transaction that has not committed yet are held back:
	// until a commit marker arrives they are not part of any consistent snapshot.
	committed := map[int][]byte{}
	pending := map[int][]byte{}
	dbSize := 0
	frames := 0

	frameSize := walFrameHeaderSize + pageSize
	for off := walHeaderSize; off+frameSize <= len(log); off += frameSize {
		hdr := log[off : off+walFrameHeaderSize]

		// A salt mismatch means this frame predates the last checkpoint reset.
		// Everything from here on is leftover, so the valid region ends.
		if !bytes.Equal(hdr[8:16], salt) {
			break
		}

		page := log[off+walFrameHeaderSize : off+frameSize]
		c0, c1 := walChecksum(sum0, sum1, hdr[:8], bigEndianSums)
		c0, c1 = walChecksum(c0, c1, page, bigEndianSums)
		if c0 != binary.BigEndian.Uint32(hdr[16:20]) || c1 != binary.BigEndian.Uint32(hdr[20:24]) {
			// Either a torn write at the tail, or a leftover frame that happens to
			// carry a matching salt. Either way the chain ends here.
			break
		}
		sum0, sum1 = c0, c1
		frames++

		pgno := binary.BigEndian.Uint32(hdr[0:4])
		if pgno == 0 {
			// Page numbering starts at 1, so this cannot be a real frame even
			// though it checksummed.
			break
		}
		pending[int(pgno)] = page

		if commitSize := binary.BigEndian.Uint32(hdr[4:8]); commitSize > 0 {
			for n, p := range pending {
				committed[n] = p
			}
			pending = map[int][]byte{}
			dbSize = int(commitSize)
		}
	}

	if len(committed) == 0 {
		// No completed transaction. The main file is the whole database.
		return nil, nil
	}
	return &walIndex{pages: committed, dbSize: dbSize, frames: frames}, nil
}

// walChecksum continues SQLite's log checksum over b.
//
// The input is read as pairs of 32-bit integers and the two accumulators are
// Fibonacci-weighted sums, so every prior byte influences the result: that is what
// makes the running value usable as a chain, where a frame's checksum attests to
// every frame before it as well as itself.
//
// Lengths not a multiple of eight cannot occur here, since the algorithm is only
// ever applied to the 24-byte header, an 8-byte frame-header prefix and a page,
// but a trailing partial word is ignored rather than read out of bounds.
func walChecksum(s0, s1 uint32, b []byte, bigEndian bool) (uint32, uint32) {
	get := binary.LittleEndian.Uint32
	if bigEndian {
		get = binary.BigEndian.Uint32
	}
	for i := 0; i+8 <= len(b); i += 8 {
		s0 += get(b[i:i+4]) + s1
		s1 += get(b[i+4:i+8]) + s0
	}
	return s0, s1
}
