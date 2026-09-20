package sqlitedb

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The fixtures under testdata were produced by the sqlite3 CLI, not by this
// package, so these tests check the reader against files SQLite itself wrote
// rather than against its own idea of the format. Regenerate them with the script
// documented in testdata/README.md.
func fixture(t *testing.T, name string) *DB {
	t.Helper()
	db, err := Open(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("Open(%s): %v", name, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// collect reads a whole table into an ordered slice of key/value pairs.
func collect(t *testing.T, db *DB, table string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := db.Scan(table, func(_ int64, vals []Value) error {
		if len(vals) < 2 {
			return fmt.Errorf("row has %d columns, want 2", len(vals))
		}
		out[vals[0].Text()] = vals[1].Text()
		return nil
	})
	if err != nil {
		t.Fatalf("Scan(%s): %v", table, err)
	}
	return out
}

func TestReadsKeyValueTable(t *testing.T) {
	db := fixture(t, "kv_small.sqlite")

	if !db.HasTable("cursorDiskKV") {
		t.Fatalf("cursorDiskKV missing; found %v", db.Tables())
	}
	rows := collect(t, db, "cursorDiskKV")
	want := map[string]string{
		"bubbleId:chat-1:msg-1": `{"text":"hello","type":1}`,
		"bubbleId:chat-1:msg-2": `{"text":"hi there","type":2}`,
		"composerData:chat-1":   `{"name":"My chat"}`,
	}
	if len(rows) != len(want) {
		t.Fatalf("read %d rows, want %d: %v", len(rows), len(want), rows)
	}
	for k, v := range want {
		if rows[k] != v {
			t.Errorf("row %q = %q, want %q", k, rows[k], v)
		}
	}
}

// A chat message is routinely larger than a page. A reader that ignored overflow
// would return a truncated value and the session would simply look smaller, so
// this is checked by digest against what sqlite3 reports.
func TestOverflowChainIsFollowedExactly(t *testing.T) {
	db := fixture(t, "kv_overflow.sqlite")
	rows := collect(t, db, "cursorDiskKV")

	big, ok := rows["bubbleId:chat-1:big"]
	if !ok {
		t.Fatalf("the overflowing row is missing; got keys %v", keys(rows))
	}
	// Length and digest both come from sqlite3 on the same fixture, via
	// writefile() rather than a shell pipeline: the CLI appends a newline to
	// query output, which would make the expected digest wrong by one byte.
	if len(big) != 26800 {
		t.Errorf("overflow value is %d bytes, want 26800", len(big))
	}
	sum := sha256.Sum256([]byte(big))
	const wantSum = "6a4c941de5be695d181e897c43bf4bb02ada168b0a8de16017d549b5313bc95c"
	if got := hex.EncodeToString(sum[:]); got != wantSum {
		t.Errorf("overflow value digest = %s, want %s", got, wantSum)
	}
	// The content must be intact at both ends, not merely the right length.
	if !strings.HasPrefix(big, "line 00000 of a long tool result") {
		t.Errorf("overflow value starts %q", first(big, 40))
	}
	if !strings.HasSuffix(big, "line 00399 of a long tool result that must survive overflow paging\n") {
		t.Errorf("overflow value ends %q", last(big, 40))
	}

	// A short row in the same table must still read normally.
	if rows["bubbleId:chat-1:small"] != "tiny" {
		t.Errorf("the non-overflowing row read as %q", rows["bubbleId:chat-1:small"])
	}
}

// With 400 rows over 512-byte pages the table b-tree has interior pages above the
// leaves, so this exercises the descent rather than a single-page read. The same
// file also contains index pages, which must not be mistaken for table pages.
func TestInteriorPagesAreTraversed(t *testing.T) {
	db := fixture(t, "kv_many.sqlite")
	rows := collect(t, db, "cursorDiskKV")

	if len(rows) != 400 {
		t.Fatalf("read %d rows, want 400", len(rows))
	}
	// Every key must be present exactly once, which a mis-walked b-tree would
	// break by dropping or repeating a subtree.
	for i := 1; i <= 400; i++ {
		k := fmt.Sprintf("bubbleId:chat-1:msg-%04d", i)
		want := fmt.Sprintf(`{"text":"message %d","type":1}`, i)
		if rows[k] != want {
			t.Fatalf("row %q = %q, want %q", k, rows[k], want)
		}
	}
}

// Rows arrive in rowid order, which is what lets a caller reconstruct a
// conversation from a table that stores no timestamps for user messages.
func TestScanIsInRowIDOrder(t *testing.T) {
	db := fixture(t, "kv_many.sqlite")
	var ids []int64
	if err := db.Scan("cursorDiskKV", func(id int64, _ []Value) error {
		ids = append(ids, id)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(ids) != 400 {
		t.Fatalf("got %d rowids", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("rowids are not ascending at %d: %d then %d", i, ids[i-1], ids[i])
		}
	}
}

func TestEverySerialTypeDecodes(t *testing.T) {
	db := fixture(t, "types.sqlite")

	got := map[string]Value{}
	if err := db.Scan("vals", func(_ int64, vals []Value) error {
		if len(vals) != 2 {
			return fmt.Errorf("row has %d columns", len(vals))
		}
		got[vals[0].Text()] = vals[1]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Expectations are what sqlite3 reports for the same fixture.
	ints := map[string]int64{
		"zero":   0,
		"one":    1,
		"i8":     100,
		"i8neg":  -100,
		"i16":    30000,
		"i24":    8000000,
		"i32":    2000000000,
		"i48":    140000000000,
		"i64":    9000000000000000000,
		"i64neg": -9000000000000000000,
	}
	for name, want := range ints {
		v, ok := got[name]
		if !ok {
			t.Errorf("%s missing", name)
			continue
		}
		if v.Kind != KindInt {
			t.Errorf("%s has kind %v, want int", name, v.Kind)
		}
		if v.Int() != want {
			t.Errorf("%s = %d, want %d", name, v.Int(), want)
		}
	}

	floats := map[string]float64{"float": 3.5, "floatneg": -0.125}
	for name, want := range floats {
		v := got[name]
		if v.Kind != KindFloat {
			t.Errorf("%s has kind %v, want float", name, v.Kind)
		}
		if v.Float() != want {
			t.Errorf("%s = %v, want %v", name, v.Float(), want)
		}
	}

	if v := got["null"]; !v.IsNull() {
		t.Errorf("null column has kind %v", v.Kind)
	}
	if v := got["text"]; v.Kind != KindText || v.Text() != "hello world" {
		t.Errorf("text = %q kind %v", v.Text(), v.Kind)
	}
	// An empty string is a text value of zero length, not a NULL.
	if v := got["empty"]; v.Kind != KindText || v.Text() != "" {
		t.Errorf("empty = %q kind %v, want a zero-length text value", v.Text(), v.Kind)
	}
	if v := got["utf8"]; v.Text() != "κόσμε 世界 🌍" {
		t.Errorf("utf8 = %q", v.Text())
	}
	if v := got["blob"]; v.Kind != KindBlob {
		t.Errorf("blob has kind %v, want blob", v.Kind)
	} else if h := strings.ToUpper(hex.EncodeToString(v.Bytes())); h != "00010203FFFE" {
		t.Errorf("blob = %s, want 00010203FFFE", h)
	}
}

// A WAL database keeps recently committed rows in its -wal sidecar. Cursor holds
// its store open, so on a real machine that is where the newest conversations
// are: reading only the main file reports an empty chat table on a machine with
// hundreds of them.
func TestWALSidecarRowsAreRead(t *testing.T) {
	db := fixture(t, "wal_pending.sqlite")

	pages, frames := db.LogPages()
	if pages == 0 || frames == 0 {
		t.Fatalf("no log was applied (pages=%d frames=%d); the fixture is meant to have one", pages, frames)
	}

	rows := collect(t, db, "cursorDiskKV")

	// Rows checkpointed before the log was reset come from the main file.
	if got := rows["checkpointed"]; got != "in the main file" {
		t.Errorf("checkpointed = %q, want the main file's value", got)
	}
	// Rows committed only to the log have to appear as well.
	for k, want := range map[string]string{
		"logged-1": "only in the log",
		"logged-2": "also only in the log",
	} {
		if got := rows[k]; got != want {
			t.Errorf("row %q = %q, want %q; it lives only in the sidecar", k, got, want)
		}
	}
	// A row the log rewrote must read as the new version, not the stale one still
	// present in the main file. This is the case that made refusing WAL databases
	// look safer than reading them.
	if got := rows["superseded"]; got != "new value" {
		t.Errorf("superseded = %q, want %q: the main file's stale copy was returned", got, "new value")
	}
	// The padding rows grew the table past its original leaf page, so the log
	// supplies b-tree pages that the main file never had.
	for i := 0; i < 24; i++ {
		k := fmt.Sprintf("logged-pad-%02d", i)
		if _, ok := rows[k]; !ok {
			t.Errorf("row %q is missing; the log's new b-tree pages were not followed", k)
		}
	}
	if len(rows) != 28 {
		t.Errorf("read %d rows, want 28", len(rows))
	}
}

// Reading the main file of the same fixture without its log must produce the
// stale database, which is what proves the test above is measuring the overlay
// and not something the main file happened to contain already.
func TestWithoutItsLogTheSameFileIsStale(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := New(data)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()

	rows := collect(t, db, "cursorDiskKV")
	if _, ok := rows["logged-1"]; ok {
		t.Error("the main file already holds the logged rows; the fixture is not exercising the log")
	}
	if got := rows["superseded"]; got != "old value" {
		t.Errorf("superseded = %q, want the stale %q", got, "old value")
	}
}

// A clean shutdown checkpoints the log and removes the sidecar, leaving a header
// that still declares WAL mode. SQLite reads such a file; so must this, or the
// tool refuses every agent that has been closed properly.
func TestWALModeWithNoSidecarIsRead(t *testing.T) {
	db := fixture(t, "wal.sqlite")
	if pages, _ := db.LogPages(); pages != 0 {
		t.Errorf("applied %d log pages, but this fixture has no sidecar", pages)
	}
	if !db.HasTable("cursorDiskKV") {
		t.Fatalf("cursorDiskKV missing; found %v", db.Tables())
	}
}

// A sidecar that cannot be interpreted is refused rather than ignored: ignoring
// it would report the main file as though it were complete, which is the stale
// read this package exists to avoid.
func TestUnreadableSidecarIsRefused(t *testing.T) {
	good, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}

	damage := map[string]func([]byte){
		"signature": func(b []byte) { b[0] ^= 0xff },
		"format version": func(b []byte) {
			b[4], b[5], b[6], b[7] = 0, 0, 0, 1
		},
		"page size": func(b []byte) {
			// Claim 1024-byte pages for a 512-byte database.
			b[8], b[9], b[10], b[11] = 0, 0, 0x04, 0
		},
		"header checksum":  func(b []byte) { b[24] ^= 0xff },
		"truncated header": func(b []byte) {},
	}
	for name, corrupt := range damage {
		t.Run(name, func(t *testing.T) {
			log := append([]byte(nil), good...)
			if name == "truncated header" {
				log = log[:20]
			}
			corrupt(log)
			if _, err := NewWithLog(main, log); err == nil {
				t.Fatal("expected a refusal")
			} else if !errors.Is(err, ErrWALMode) {
				t.Errorf("error = %v, want ErrWALMode", err)
			} else if !strings.Contains(err.Error(), "log") {
				t.Errorf("the error should name the log, got %q", err)
			}
		})
	}
}

// A writer appending while this reads leaves a partial frame at the end. The scan
// has to stop at the last intact commit rather than read the fragment, so a
// concurrent write cannot turn a read into an error or into garbage.
func TestPartialTailFrameIsIgnored(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	full, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}

	// Cut mid-frame, keeping the first transactions whole.
	const frameSize = 24 + 512
	cut := walHeaderSize + 2*frameSize + 100
	if cut >= len(full) {
		t.Fatalf("fixture log is only %d bytes, too short for this test", len(full))
	}
	db, err := NewWithLog(main, full[:cut])
	if err != nil {
		t.Fatalf("a torn tail should not fail the read: %v", err)
	}
	defer db.Close()

	rows := collect(t, db, "cursorDiskKV")
	// The first log transaction committed within the kept bytes, so its row is
	// visible while the later ones are not.
	if got := rows["logged-1"]; got != "only in the log" {
		t.Errorf("logged-1 = %q, want the row from the last intact commit", got)
	}
	if _, ok := rows["logged-pad-00"]; ok {
		t.Error("a row from beyond the truncation point was returned")
	}
}

// Frames left over from before a checkpoint carry the previous salt. They must be
// ignored, or a reader resurrects pages the database has moved past.
func TestStaleFramesAfterACheckpointAreIgnored(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(filepath.Join("testdata", "wal_pending.sqlite-wal"))
	if err != nil {
		t.Fatal(err)
	}
	log = append([]byte(nil), log...)

	// Change the salt in the third frame's header, as a WAL reset would.
	const frameSize = 24 + 512
	third := walHeaderSize + 2*frameSize
	log[third+8] ^= 0xff

	db, err := NewWithLog(main, log)
	if err != nil {
		t.Fatalf("NewWithLog: %v", err)
	}
	defer db.Close()

	_, frames := db.LogPages()
	if frames != 2 {
		t.Errorf("read %d frames, want 2: the scan should end at the salt change", frames)
	}
}

// A database whose header does not declare WAL mode has no log, so a file that
// happens to sit beside it under that name is not one and must not be applied.
func TestSidecarIsIgnoredForNonWALDatabase(t *testing.T) {
	main, err := os.ReadFile(filepath.Join("testdata", "kv_small.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := NewWithLog(main, []byte("not a write-ahead log at all, and long enough to parse"))
	if err != nil {
		t.Fatalf("a rollback-journal database should ignore a stray sidecar: %v", err)
	}
	defer db.Close()
	if pages, _ := db.LogPages(); pages != 0 {
		t.Errorf("applied %d pages from a file that is not this database's log", pages)
	}
}

func TestNonDatabaseInputIsRejected(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"empty.db":  {},
		"short.db":  []byte("SQLite for"),
		"json.db":   []byte(`{"not":"a database"}`),
		"almost.db": append([]byte("SQLite format 4\x00"), make([]byte, 200)...),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(p); err == nil {
				t.Error("expected a rejection")
			} else if !errors.Is(err, ErrNotSQLite) {
				t.Errorf("error = %v, want ErrNotSQLite", err)
			}
		})
	}
}

func TestMissingTableNamesWhatWasFound(t *testing.T) {
	db := fixture(t, "kv_small.sqlite")
	err := db.Scan("nope", func(int64, []Value) error { return nil })
	if err == nil {
		t.Fatal("scanning a missing table should fail")
	}
	// The adapter turns this into a diagnostic, so it has to list the real tables.
	if !strings.Contains(err.Error(), "cursorDiskKV") {
		t.Errorf("the error should name the tables present, got %q", err)
	}
}

func TestColumnNamesAreParsedFromDDL(t *testing.T) {
	db := fixture(t, "kv_small.sqlite")
	got := db.Columns("cursorDiskKV")
	want := []string{"key", "value"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Columns = %v, want %v", got, want)
	}

	// Table-level constraints are not columns, and a quoted name keeps its
	// spelling minus the quotes.
	ddl := fixture(t, "ddl.sqlite")
	got = ddl.Columns("items")
	want = []string{"id", "quoted name", "amount"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Columns(items) = %v, want %v", got, want)
	}
}

// Stopping early has to work, so a caller reading one conversation out of a large
// store does not have to walk the whole table.
func TestScanStopsOnCallerError(t *testing.T) {
	db := fixture(t, "kv_many.sqlite")
	sentinel := errors.New("enough")
	seen := 0
	err := db.Scan("cursorDiskKV", func(int64, []Value) error {
		seen++
		if seen == 5 {
			return sentinel
		}
		return nil
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("Scan returned %v, want the caller's error", err)
	}
	if seen != 5 {
		t.Errorf("callback ran %d times after asking to stop at 5", seen)
	}
}

func TestVarintMatchesTheSpecification(t *testing.T) {
	cases := []struct {
		in   []byte
		want int64
		n    int
	}{
		{[]byte{0x00}, 0, 1},
		{[]byte{0x7f}, 127, 1},
		{[]byte{0x81, 0x00}, 128, 2},
		{[]byte{0x82, 0x21}, 289, 2},
		{[]byte{0xff, 0x7f}, 16383, 2},
		{[]byte{0x81, 0x80, 0x00}, 16384, 3},
		// Nine bytes: the last contributes all eight bits.
		{[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, -1, 9},
	}
	for _, tc := range cases {
		got, n := varint(tc.in)
		if got != tc.want || n != tc.n {
			t.Errorf("varint(% x) = %d/%d, want %d/%d", tc.in, got, n, tc.want, tc.n)
		}
	}
	// A truncated varint reports zero bytes consumed rather than a wrong value.
	if _, n := varint([]byte{0x81}); n != 0 {
		t.Errorf("a truncated varint should consume 0 bytes, got %d", n)
	}
	if _, n := varint(nil); n != 0 {
		t.Errorf("an empty varint should consume 0 bytes, got %d", n)
	}
}

// Corruption must surface as an error. A reader that skipped unreadable pages
// would report a smaller session instead of a failure.
func TestCorruptionIsAnErrorNotASilentShortRead(t *testing.T) {
	original, err := os.ReadFile(filepath.Join("testdata", "kv_many.sqlite"))
	if err != nil {
		t.Fatal(err)
	}

	// Truncating mid-table removes pages the b-tree still points at.
	t.Run("truncated", func(t *testing.T) {
		cut := len(original) / 2
		db, err := New(original[:cut-(cut%512)])
		if err != nil {
			return // Refused at open, which is also correct.
		}
		if err := db.Scan("cursorDiskKV", func(int64, []Value) error { return nil }); err == nil {
			t.Error("scanning a truncated database should fail rather than return fewer rows")
		}
	})

	// A b-tree page claiming an impossible cell count must not be trusted.
	t.Run("absurd cell count", func(t *testing.T) {
		bad := make([]byte, len(original))
		copy(bad, original)
		db, err := New(bad)
		if err != nil {
			t.Skipf("fixture did not open: %v", err)
		}
		var root int
		for _, name := range db.Tables() {
			if name == "cursorDiskKV" {
				root = db.tables[name].rootPage
			}
		}
		if root == 0 {
			t.Skip("table root not found")
		}
		off := (root - 1) * db.pageSize
		bad[off+3], bad[off+4] = 0xff, 0xff // 65535 cells in a 512-byte page
		db2, err := New(bad)
		if err != nil {
			return
		}
		if err := db2.Scan("cursorDiskKV", func(int64, []Value) error { return nil }); err == nil {
			t.Error("an impossible cell count should be rejected")
		}
	})
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func first(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func last(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
