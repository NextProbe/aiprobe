package sqlitedb

import (
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A binary parser is new attack surface. AIProbe reads a database written by
// another application, which may be truncated by a crash, half-checkpointed, or
// simply corrupt on failing storage. None of that may panic, hang or read out of
// bounds: it has to come back as an error.
//
// This is a deterministic mutation sweep rather than a Go fuzz target so it runs
// in the normal suite. The seed is fixed, so a failure is reproducible.
func TestMutatedDatabasesNeverPanic(t *testing.T) {
	original, err := os.ReadFile(filepath.Join("testdata", "kv_many.sqlite"))
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(20260917))
	deadline := time.Now().Add(20 * time.Second)

	for i := 0; i < 3000; i++ {
		if time.Now().After(deadline) {
			t.Logf("stopped after %d mutations to stay inside the time budget", i)
			break
		}
		bad := make([]byte, len(original))
		copy(bad, original)

		// Flip a handful of bytes anywhere in the file, including the header and
		// the b-tree structures.
		for f := 0; f < 1+rng.Intn(6); f++ {
			bad[rng.Intn(len(bad))] = byte(rng.Intn(256))
		}
		// Sometimes truncate as well, which is what an interrupted write looks like.
		if rng.Intn(4) == 0 && len(bad) > 512 {
			bad = bad[:rng.Intn(len(bad))]
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					// Write the input out so the failure can be reproduced.
					name := filepath.Join(t.TempDir(), "panic.sqlite")
					_ = os.WriteFile(name, bad, 0o600)
					t.Fatalf("mutation %d panicked: %v (input saved to %s)", i, r, name)
				}
			}()

			db, err := New(bad)
			if err != nil {
				return // Refused at open, which is a correct outcome.
			}
			defer db.Close()

			// Walking the catalogue and every table must also be panic-free. The
			// row callback does what a real caller does: read the columns.
			for _, table := range db.Tables() {
				_ = db.Columns(table)
				_ = db.Scan(table, func(_ int64, vals []Value) error {
					for _, v := range vals {
						_ = v.Text()
						_ = v.Int()
						_ = v.Float()
						_ = v.Bytes()
						_ = v.IsNull()
					}
					return nil
				})
			}
		}()
	}
}

// Random bytes that merely start with the signature must not be mistaken for a
// database, and must not crash the header parse.
func TestRandomBodyBehindAValidSignature(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 2000; i++ {
		buf := make([]byte, 100+rng.Intn(4096))
		copy(buf, magic)
		for j := len(magic); j < len(buf); j++ {
			buf[j] = byte(rng.Intn(256))
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d panicked: %v", i, r)
				}
			}()
			db, err := New(buf)
			if err != nil {
				return
			}
			defer db.Close()
			for _, table := range db.Tables() {
				_ = db.Scan(table, func(int64, []Value) error { return nil })
			}
		}()
	}
}

// The write-ahead log is the second binary parser here, and the more exposed of
// the two: it is read while another process is appending to it, so a torn frame is
// the normal case rather than a corruption scenario. Frame offsets are derived
// from a page size the file itself declares, which is exactly the shape of bug
// that reads out of bounds.
//
// The log alone is mutated as well as the pair, because a valid database with a
// damaged log is the realistic failure and the one where a wrong answer would be
// silent rather than loud.
func TestMutatedWriteAheadLogsNeverPanic(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}

	rng := rand.New(rand.NewSource(20260918))
	deadline := time.Now().Add(20 * time.Second)

	for i := 0; i < 4000; i++ {
		if time.Now().After(deadline) {
			t.Logf("stopped after %d mutations to stay inside the time budget", i)
			break
		}

		badMain := append([]byte(nil), main...)
		badLog := append([]byte(nil), log...)

		// Most iterations leave the database intact, since a good database with a
		// bad log is the case that matters.
		if rng.Intn(4) == 0 {
			for f := 0; f < 1+rng.Intn(4); f++ {
				badMain[rng.Intn(len(badMain))] = byte(rng.Intn(256))
			}
		}
		// Mutations are biased past the 32-byte header, because a damaged header is
		// caught by its own checksum in a few instructions and never reaches the
		// frame loop. Leaving the header alone is what actually exercises the frame
		// walk, the salt comparison and the page-size arithmetic. A few iterations
		// still hit the header, so that path is covered too.
		from := walHeaderSize
		if rng.Intn(10) == 0 {
			from = 0
		}
		for f := 0; f < 1+rng.Intn(8); f++ {
			badLog[from+rng.Intn(len(badLog)-from)] = byte(rng.Intn(256))
		}
		// Truncation anywhere, including inside the header, which is what a log
		// being created right now looks like.
		if rng.Intn(3) == 0 {
			badLog = badLog[:rng.Intn(len(badLog))]
		}

		func() {
			defer func() {
				if r := recover(); r != nil {
					dir := t.TempDir()
					_ = os.WriteFile(filepath.Join(dir, "panic.sqlite"), badMain, 0o600)
					_ = os.WriteFile(filepath.Join(dir, "panic.sqlite-wal"), badLog, 0o600)
					t.Fatalf("mutation %d panicked: %v (input saved to %s)", i, r, dir)
				}
			}()

			db, err := NewWithLog(badMain, badLog)
			if err != nil {
				return // Refused at open, which is a correct outcome.
			}
			defer db.Close()

			for _, table := range db.Tables() {
				_ = db.Columns(table)
				_ = db.Scan(table, func(_ int64, vals []Value) error {
					for _, v := range vals {
						_ = v.Text()
						_ = v.Int()
						_ = v.Float()
						_ = v.Bytes()
						_ = v.IsNull()
					}
					return nil
				})
			}
		}()
	}
}

// A log made entirely of random bytes behind a valid signature must be refused or
// read as empty, never trusted. The page size and frame count come from the file,
// so this is where a bad stride would show up.
func TestRandomWriteAheadLogBody(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 3000; i++ {
		buf := make([]byte, 32+rng.Intn(4096))
		for j := range buf {
			buf[j] = byte(rng.Intn(256))
		}
		// Half the time, give it a header SQLite would accept, so the frame loop is
		// actually reached rather than rejected at the signature.
		if i%2 == 0 {
			copy(buf[0:4], []byte{0x37, 0x7f, 0x06, 0x83})
			copy(buf[4:8], []byte{0x00, 0x2d, 0xe2, 0x18}) // 3007000
			copy(buf[8:12], []byte{0x00, 0x00, 0x02, 0x00})
			s0, s1 := walChecksum(0, 0, buf[:24], true)
			putBE32(buf[24:28], s0)
			putBE32(buf[28:32], s1)
		}
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("iteration %d panicked: %v", i, r)
				}
			}()
			db, err := NewWithLog(main, buf)
			if err != nil {
				return
			}
			defer db.Close()
			for _, table := range db.Tables() {
				_ = db.Scan(table, func(int64, []Value) error { return nil })
			}
		}()
	}
}

func putBE32(b []byte, v uint32) {
	b[0] = byte(v >> 24)
	b[1] = byte(v >> 16)
	b[2] = byte(v >> 8)
	b[3] = byte(v)
}

// resealLog recomputes a log's header checksum and the whole frame checksum
// chain, so a mutated log is accepted as valid.
//
// Mutating a log without this only ever produces a torn tail: the frame's own
// checksum rejects it and the scan stops, which exercises the reader's cheapest
// path. Re-sealing is what produces the dangerous input — a frame the reader
// believes, carrying a hostile page number or a page that is not a b-tree page at
// all — and that is the only way the overlay reaches the traversal code with
// attacker-shaped bytes.
func resealLog(log []byte, pageSize int) {
	if len(log) < walHeaderSize {
		return
	}
	// Force the big-endian-checksum signature so the test does not have to care
	// which one a mutation produced.
	copy(log[0:4], []byte{0x37, 0x7f, 0x06, 0x83})
	putBE32(log[4:8], walFormat)
	putBE32(log[8:12], uint32(pageSize))

	s0, s1 := walChecksum(0, 0, log[:24], true)
	putBE32(log[24:28], s0)
	putBE32(log[28:32], s1)

	salt := log[16:24]
	frameSize := walFrameHeaderSize + pageSize
	for off := walHeaderSize; off+frameSize <= len(log); off += frameSize {
		hdr := log[off : off+walFrameHeaderSize]
		copy(hdr[8:16], salt)
		c0, c1 := walChecksum(s0, s1, hdr[:8], true)
		c0, c1 = walChecksum(c0, c1, log[off+walFrameHeaderSize:off+frameSize], true)
		putBE32(hdr[16:20], c0)
		putBE32(hdr[20:24], c1)
		s0, s1 = c0, c1
	}
}

// Frames the reader accepts may still contain nonsense: a page number outside the
// database, a page that is an index where a table was expected, a b-tree header
// describing more cells than fit. The overlay hands those straight to the same
// traversal the main file uses, so all of it has to come back as an error.
func TestAcceptedButHostileLogFramesNeverPanic(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}

	const pageSize = 512
	rng := rand.New(rand.NewSource(20260919))
	deadline := time.Now().Add(20 * time.Second)
	accepted := 0

	for i := 0; i < 4000; i++ {
		if time.Now().After(deadline) {
			t.Logf("stopped after %d mutations to stay inside the time budget", i)
			break
		}
		bad := append([]byte(nil), log...)

		for f := 0; f < 1+rng.Intn(10); f++ {
			bad[walHeaderSize+rng.Intn(len(bad)-walHeaderSize)] = byte(rng.Intn(256))
		}
		// Sometimes point a frame at an absurd page, which is the case that would
		// index outside the file if the bound were missing.
		if rng.Intn(3) == 0 {
			frameSize := walFrameHeaderSize + pageSize
			frames := (len(bad) - walHeaderSize) / frameSize
			if frames > 0 {
				off := walHeaderSize + rng.Intn(frames)*frameSize
				putBE32(bad[off:off+4], rng.Uint32())
			}
		}
		resealLog(bad, pageSize)

		func() {
			defer func() {
				if r := recover(); r != nil {
					dir := t.TempDir()
					_ = os.WriteFile(filepath.Join(dir, "panic.sqlite"), main, 0o600)
					_ = os.WriteFile(filepath.Join(dir, "panic.sqlite-wal"), bad, 0o600)
					t.Fatalf("mutation %d panicked: %v (input saved to %s)", i, r, dir)
				}
			}()

			db, err := NewWithLog(main, bad)
			if err != nil {
				return
			}
			defer db.Close()
			accepted++

			for _, table := range db.Tables() {
				_ = db.Columns(table)
				_ = db.Scan(table, func(_ int64, vals []Value) error {
					for _, v := range vals {
						_ = v.Text()
						_ = v.Bytes()
					}
					return nil
				})
			}
		}()
	}

	// If re-sealing stopped working the sweep would pass while testing nothing, so
	// the number of inputs that actually got through is asserted rather than
	// assumed.
	if accepted < 100 {
		t.Errorf("only %d mutated logs were accepted; the sweep is not reaching the traversal", accepted)
	}
}
