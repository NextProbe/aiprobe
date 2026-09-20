// Package sqlitedb is a minimal, read-only SQLite file reader.
//
// # Why this exists
//
// Four of the five runtimes AIProbe reads store sessions as JSON on disk. Cursor
// does not: its chat history lives only in a SQLite database, so supporting it
// means either linking a SQLite driver or reading the file format directly.
//
// A pure-Go SQLite driver takes the dependency count from one to twenty-six,
// most of it a C-to-Go translation of libc. For a tool whose pitch is that it is
// local, read-only and auditable in an afternoon, that is a bad trade for one
// adapter. So this package reads the bytes.
//
// # What it deliberately is not
//
// This is not a SQLite implementation. There is no SQL, no query planner, no
// write path, no index support, no transactions, no collations, no savepoints.
// The entire surface is "walk one table's rows", which is all any adapter needs.
//
// What it does implement, because the format requires it to read real files:
// the database header, the sqlite_master catalogue, table B-tree traversal for
// both interior and leaf pages, the record format with all serial types, and
// overflow page chains. Overflow is not optional: a chat message is routinely
// larger than a page, so a reader that ignored overflow would silently truncate
// the interesting rows.
//
// # The write-ahead log is read, not refused
//
// A database in WAL mode keeps recently committed pages in a -wal sidecar, and
// Cursor's store is in WAL mode permanently because the editor holds it open. So
// the sidecar is recovered and overlaid on the main file; see wal.go. Refusing to
// read WAL databases, which is what this package used to do, meant refusing every
// Cursor installation that had ever been used.
//
// When the sidecar is absent the main file is read as it stands, which is what
// SQLite itself does: a clean shutdown checkpoints the log and removes it.
//
// # Refusing rather than guessing
//
// Two cases return errors instead of partial data.
//
// A -wal sidecar that exists but cannot be interpreted is refused rather than
// ignored, because ignoring it would report the main file as though it were
// complete.
//
// Anything the format says is impossible (a cell pointer outside the page, a
// cyclic B-tree, a truncated record) is an error, not a skipped row. A corrupt
// read that quietly returns fewer rows would show up as a smaller session rather
// than as a failure.
package sqlitedb

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

// headerSize is the fixed database header, which occupies the start of page 1.
const headerSize = 100

// magic is the string every SQLite file begins with, including the NUL.
const magic = "SQLite format 3\x00"

// B-tree page types.
const (
	pageInteriorIndex = 0x02
	pageInteriorTable = 0x05
	pageLeafIndex     = 0x0a
	pageLeafTable     = 0x0d
)

// ErrNotSQLite is returned for a file that is not a SQLite database.
var ErrNotSQLite = errors.New("not a SQLite database")

// ErrWALMode is returned when a database's -wal sidecar exists but cannot be
// interpreted, so the main file cannot be trusted to be complete.
var ErrWALMode = errors.New("write-ahead log cannot be read")

// DB is an open read-only database.
type DB struct {
	data []byte
	// wal holds pages the log supersedes, or nil when the main file is complete.
	wal *walIndex

	pageSize   int
	usableSize int
	pageCount  int
	encoding   int

	tables map[string]tableDef
}

// tableDef is one row of the sqlite_master catalogue.
type tableDef struct {
	name     string
	rootPage int
	sql      string
}

// Open reads a database, and its write-ahead log when it has one, into memory.
//
// The whole file is read at once rather than paged in on demand. These databases
// are small enough for that in the case this package exists to serve, and it
// removes every possibility of the file changing under a partial read, which
// matters when the owning application may be running.
//
// The log is read after the main file so that a checkpoint racing this call
// cannot hide pages from both: a checkpoint moves pages from the log into the
// database, so anything missing from the snapshot of the main file is still in the
// log when it is read a moment later.
func Open(path string) (*DB, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	log, err := os.ReadFile(path + walSuffix)
	if err != nil && !os.IsNotExist(err) {
		// The sidecar is there but unreadable, most often because another user
		// owns it. Reporting the main file as complete would be a stale read.
		return nil, fmt.Errorf("%w: %v", ErrWALMode, err)
	}
	return NewWithLog(data, log)
}

// New reads a database from bytes already in memory, with no write-ahead log.
func New(data []byte) (*DB, error) { return NewWithLog(data, nil) }

// NewWithLog reads a database and the contents of its -wal sidecar. A nil or
// empty log means the main file stands alone.
func NewWithLog(data, log []byte) (*DB, error) {
	if len(data) < headerSize {
		return nil, fmt.Errorf("%w: file is %d bytes, shorter than the header", ErrNotSQLite, len(data))
	}
	if string(data[:len(magic)]) != magic {
		return nil, fmt.Errorf("%w: missing the format 3 signature", ErrNotSQLite)
	}

	db := &DB{data: data}

	// Page size lives at offset 16. The value 1 means 65536, which does not fit
	// in the two bytes available.
	switch ps := int(binary.BigEndian.Uint16(data[16:18])); ps {
	case 1:
		db.pageSize = 65536
	default:
		db.pageSize = ps
	}
	if db.pageSize < 512 || db.pageSize&(db.pageSize-1) != 0 {
		return nil, fmt.Errorf("%w: page size %d is not a power of two of at least 512", ErrNotSQLite, db.pageSize)
	}

	// Reserved space at the end of every page, normally zero.
	reserved := int(data[20])
	db.usableSize = db.pageSize - reserved
	if db.usableSize < 480 {
		return nil, fmt.Errorf("%w: usable page size %d is too small", ErrNotSQLite, db.usableSize)
	}

	db.pageCount = int(binary.BigEndian.Uint32(data[28:32]))
	if db.pageCount <= 0 || db.pageCount*db.pageSize > len(data) {
		// The in-header count is only authoritative when it agrees with the file;
		// older writers left it stale, so fall back to the actual length.
		db.pageCount = len(data) / db.pageSize
	}
	if db.pageCount == 0 {
		return nil, fmt.Errorf("%w: no complete pages", ErrNotSQLite)
	}

	// A file written for WAL declares read/write format version 2. Only then can
	// a sidecar hold pages that supersede this file, so only then is one applied:
	// a stray -wal beside a rollback-journal database is not this database's log.
	if data[18] == 2 || data[19] == 2 {
		idx, err := readWAL(log, db.pageSize)
		if err != nil {
			return nil, err
		}
		if idx != nil {
			db.wal = idx
			// The log's last commit records the true size. A WAL-mode writer does
			// not keep the main file's header count current, and the database may
			// have grown into pages that exist only in the log.
			if idx.dbSize > 0 {
				db.pageCount = idx.dbSize
			}
		}
	}

	db.encoding = int(binary.BigEndian.Uint32(data[56:60]))
	if db.encoding == 0 {
		db.encoding = 1
	}
	if db.encoding != 1 {
		// UTF-16 text would need transcoding. No VS Code derivative writes it, so
		// this is refused rather than half-supported.
		return nil, fmt.Errorf("%w: text encoding %d is not UTF-8", ErrNotSQLite, db.encoding)
	}

	if err := db.readCatalogue(); err != nil {
		return nil, err
	}
	return db, nil
}

// Close releases the database. Present so callers can defer it regardless of how
// the reader is implemented.
func (d *DB) Close() error {
	d.data = nil
	d.wal = nil
	d.tables = nil
	return nil
}

// PageSize reports the database page size.
func (d *DB) PageSize() int { return d.pageSize }

// LogPages reports how many pages were taken from the write-ahead log rather than
// the main file, and how many valid frames it held. Both are zero when there was
// no log to apply. Exposed so a caller can say where the rows came from.
func (d *DB) LogPages() (pages, frames int) {
	if d.wal == nil {
		return 0, 0
	}
	return len(d.wal.pages), d.wal.frames
}

// page returns the bytes of a 1-indexed page.
//
// The log is consulted first, because a page it holds is a later version of the
// one in the main file. That is the whole of the WAL overlay: every read goes
// through here, so nothing else in the reader has to know the log exists.
func (d *DB) page(n int) ([]byte, error) {
	if n < 1 || n > d.pageCount {
		return nil, fmt.Errorf("page %d is outside the %d page database", n, d.pageCount)
	}
	if d.wal != nil {
		if p, ok := d.wal.pages[n]; ok {
			return p, nil
		}
	}
	start := (n - 1) * d.pageSize
	if start >= len(d.data) {
		// Only reachable when the log's commit record claims a larger database
		// than the file holds and the missing page is not in the log either.
		return nil, fmt.Errorf("page %d is past the end of the file and not in the write-ahead log", n)
	}
	end := start + d.pageSize
	if end > len(d.data) {
		end = len(d.data)
	}
	return d.data[start:end], nil
}

// readCatalogue loads sqlite_master, whose root is always page 1.
func (d *DB) readCatalogue() error {
	d.tables = map[string]tableDef{}
	return d.walk(1, func(_ int64, vals []Value) error {
		// sqlite_master is (type, name, tbl_name, rootpage, sql).
		if len(vals) < 4 {
			return nil
		}
		if vals[0].Text() != "table" {
			return nil
		}
		name := vals[1].Text()
		if name == "" {
			return nil
		}
		def := tableDef{name: name, rootPage: int(vals[3].Int())}
		if len(vals) >= 5 {
			def.sql = vals[4].Text()
		}
		d.tables[name] = def
		return nil
	})
}

// Tables lists the table names in the database, which is what an adapter needs to
// report when the schema is not what it expected.
func (d *DB) Tables() []string {
	out := make([]string, 0, len(d.tables))
	for name := range d.tables {
		out = append(out, name)
	}
	return out
}

// HasTable reports whether a table exists.
func (d *DB) HasTable(name string) bool {
	_, ok := d.tables[name]
	return ok
}

// Columns returns the declared column names of a table.
//
// The names come from a deliberately small parse of the stored CREATE TABLE
// text: the outermost parentheses, split on top-level commas, first identifier of
// each part. That covers the simple key/value schemas this package is used for
// and returns nil for anything more elaborate, so callers fall back to positional
// access rather than trusting a half-understood DDL.
func (d *DB) Columns(table string) []string {
	def, ok := d.tables[table]
	if !ok {
		return nil
	}
	open := strings.IndexByte(def.sql, '(')
	if open < 0 {
		return nil
	}
	body := def.sql[open+1:]
	if close := strings.LastIndexByte(body, ')'); close >= 0 {
		body = body[:close]
	}

	var cols []string
	depth := 0
	start := 0
	flush := func(part string) {
		part = strings.TrimSpace(part)
		if part == "" {
			return
		}
		// A table-level constraint is not a column. A quoted identifier always is
		// one, even when it spells a keyword, so the check only applies to bare
		// words.
		switch part[0] {
		case '"', '`', '\'', '[':
		default:
			switch strings.ToUpper(firstWord(part)) {
			case "PRIMARY", "UNIQUE", "CHECK", "FOREIGN", "CONSTRAINT":
				return
			}
		}
		cols = append(cols, columnName(part))
	}
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				flush(body[start:i])
				start = i + 1
			}
		}
	}
	flush(body[start:])
	return cols
}

// columnName reads the identifier at the start of a column definition.
//
// A quoted identifier is read to its closing quote rather than to the first
// space, because SQLite permits spaces inside one and truncating at the space
// would silently rename the column.
func columnName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var closer byte
	switch s[0] {
	case '"':
		closer = '"'
	case '`':
		closer = '`'
	case '\'':
		closer = '\''
	case '[':
		closer = ']'
	}
	if closer != 0 {
		if end := strings.IndexByte(s[1:], closer); end >= 0 {
			return s[1 : 1+end]
		}
		return s[1:]
	}
	return firstWord(s)
}

func firstWord(s string) string {
	s = strings.TrimSpace(s)
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\n', '\r', '(':
			return s[:i]
		}
	}
	return s
}

// Scan calls fn for every row of a table, in B-tree order.
//
// Returning an error from fn stops the scan and is returned to the caller, so a
// consumer can stop early without reading the rest of a large table.
func (d *DB) Scan(table string, fn func(rowID int64, vals []Value) error) error {
	def, ok := d.tables[table]
	if !ok {
		return fmt.Errorf("no table %q in this database (found: %s)", table, strings.Join(d.Tables(), ", "))
	}
	if def.rootPage < 1 {
		// A rootpage of 0 means a view or virtual table, which has no rows here.
		return fmt.Errorf("table %q has no root page; it is not a plain table", table)
	}
	return d.walk(def.rootPage, fn)
}

// walk traverses a table B-tree from a root page.
func (d *DB) walk(root int, fn func(int64, []Value) error) error {
	// A malformed or hostile file could describe a cycle. Tracking visited pages
	// bounds the traversal instead of hanging.
	visited := map[int]bool{}
	return d.walkPage(root, fn, visited, 0)
}

// maxBTreeDepth bounds recursion. SQLite's own limit is far below this; a deeper
// tree means the file is not what it claims to be.
const maxBTreeDepth = 64

func (d *DB) walkPage(n int, fn func(int64, []Value) error, visited map[int]bool, depth int) error {
	if depth > maxBTreeDepth {
		return fmt.Errorf("b-tree deeper than %d levels", maxBTreeDepth)
	}
	if visited[n] {
		return fmt.Errorf("b-tree page %d is reachable twice; the file is corrupt", n)
	}
	visited[n] = true

	pg, err := d.page(n)
	if err != nil {
		return err
	}

	// Page 1 carries the database header before its B-tree header.
	hdr := 0
	if n == 1 {
		hdr = headerSize
	}
	if len(pg) < hdr+8 {
		return fmt.Errorf("page %d is too short for a b-tree header", n)
	}

	typ := pg[hdr]
	cells := int(binary.BigEndian.Uint16(pg[hdr+3 : hdr+5]))

	headerLen := 8
	if typ == pageInteriorTable || typ == pageInteriorIndex {
		headerLen = 12
	}
	switch typ {
	case pageLeafTable, pageInteriorTable:
	case pageLeafIndex, pageInteriorIndex:
		// Indexes carry no table rows. Reaching one means a root page was
		// misidentified, which is a bug rather than something to skip silently.
		return fmt.Errorf("page %d is an index page; expected a table b-tree", n)
	default:
		return fmt.Errorf("page %d has unknown b-tree type 0x%02x", n, typ)
	}

	ptrStart := hdr + headerLen
	if ptrStart+cells*2 > len(pg) {
		return fmt.Errorf("page %d declares %d cells, more than fit", n, cells)
	}

	for i := 0; i < cells; i++ {
		off := int(binary.BigEndian.Uint16(pg[ptrStart+i*2 : ptrStart+i*2+2]))
		if off < 0 || off >= len(pg) {
			return fmt.Errorf("page %d cell %d points to offset %d, outside the page", n, i, off)
		}
		cell := pg[off:]

		if typ == pageInteriorTable {
			if len(cell) < 4 {
				return fmt.Errorf("page %d cell %d is truncated", n, i)
			}
			child := int(binary.BigEndian.Uint32(cell[:4]))
			if err := d.walkPage(child, fn, visited, depth+1); err != nil {
				return err
			}
			continue
		}

		// Leaf table cell: payload length, rowid, payload.
		payloadLen, k := varint(cell)
		if k == 0 {
			return fmt.Errorf("page %d cell %d has an unreadable payload length", n, i)
		}
		cell = cell[k:]
		rowID, k := varint(cell)
		if k == 0 {
			return fmt.Errorf("page %d cell %d has an unreadable rowid", n, i)
		}
		cell = cell[k:]

		payload, err := d.payload(cell, int(payloadLen))
		if err != nil {
			return fmt.Errorf("page %d cell %d: %w", n, i, err)
		}
		vals, err := decodeRecord(payload)
		if err != nil {
			return fmt.Errorf("page %d cell %d: %w", n, i, err)
		}
		if err := fn(rowID, vals); err != nil {
			return err
		}
	}

	// An interior page's right-most child is in its header, not the cell array.
	if typ == pageInteriorTable {
		right := int(binary.BigEndian.Uint32(pg[hdr+8 : hdr+12]))
		if right != 0 {
			if err := d.walkPage(right, fn, visited, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

// payload assembles a cell payload, following the overflow chain when the row is
// larger than a page can hold.
//
// The local-size arithmetic is from the file format specification. It exists so
// that a row slightly larger than a page does not waste an almost-empty overflow
// page, which means the split point is not simply "one page worth".
func (d *DB) payload(cell []byte, total int) ([]byte, error) {
	if total < 0 {
		return nil, fmt.Errorf("negative payload length %d", total)
	}
	maxLocal := d.usableSize - 35
	if total <= maxLocal {
		if total > len(cell) {
			return nil, fmt.Errorf("payload of %d bytes exceeds the %d remaining in the page", total, len(cell))
		}
		return cell[:total], nil
	}

	minLocal := ((d.usableSize-12)*32)/255 - 23
	local := minLocal + ((total - minLocal) % (d.usableSize - 4))
	if local > maxLocal {
		local = minLocal
	}
	if local < 0 || local+4 > len(cell) {
		return nil, fmt.Errorf("overflow cell is truncated: need %d bytes plus a page pointer, have %d", local, len(cell))
	}

	out := make([]byte, 0, total)
	out = append(out, cell[:local]...)
	next := int(binary.BigEndian.Uint32(cell[local : local+4]))

	// Bound the chain by the database size: a cycle would otherwise never end.
	for hops := 0; next != 0 && len(out) < total; hops++ {
		if hops > d.pageCount {
			return nil, errors.New("overflow chain is longer than the database; the file is corrupt")
		}
		pg, err := d.page(next)
		if err != nil {
			return nil, fmt.Errorf("overflow page: %w", err)
		}
		if len(pg) < 4 {
			return nil, errors.New("overflow page is truncated")
		}
		chunk := pg[4:]
		if limit := d.usableSize - 4; len(chunk) > limit {
			chunk = chunk[:limit]
		}
		if need := total - len(out); len(chunk) > need {
			chunk = chunk[:need]
		}
		out = append(out, chunk...)
		next = int(binary.BigEndian.Uint32(pg[:4]))
	}
	if len(out) != total {
		return nil, fmt.Errorf("assembled %d of %d payload bytes", len(out), total)
	}
	return out, nil
}

// varint decodes SQLite's big-endian variable-length integer. It returns the
// value and the bytes consumed, or 0 bytes when the input is truncated.
//
// The first eight bytes contribute seven bits each; a ninth contributes all
// eight, which is how a full 64-bit value fits.
func varint(b []byte) (int64, int) {
	var v uint64
	for i := 0; i < 8; i++ {
		if i >= len(b) {
			return 0, 0
		}
		v = v<<7 | uint64(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return int64(v), i + 1
		}
	}
	if len(b) < 9 {
		return 0, 0
	}
	return int64(v<<8 | uint64(b[8])), 9
}

// Kind classifies a column value.
type Kind int

// Value kinds.
const (
	KindNull Kind = iota
	KindInt
	KindFloat
	KindText
	KindBlob
)

// Value is one column of one row.
type Value struct {
	Kind  Kind
	i     int64
	f     float64
	bytes []byte
}

// Int returns the integer value, or 0.
func (v Value) Int() int64 {
	switch v.Kind {
	case KindInt:
		return v.i
	case KindFloat:
		return int64(v.f)
	}
	return 0
}

// Float returns the floating point value, or 0.
func (v Value) Float() float64 {
	switch v.Kind {
	case KindFloat:
		return v.f
	case KindInt:
		return float64(v.i)
	}
	return 0
}

// Text returns the value as a string. Blobs are returned as-is, because the
// applications this reader serves store JSON in blob columns as often as in text
// ones, and refusing to read it would be pedantry.
func (v Value) Text() string {
	switch v.Kind {
	case KindText, KindBlob:
		return string(v.bytes)
	case KindInt:
		return fmt.Sprintf("%d", v.i)
	case KindFloat:
		return fmt.Sprintf("%g", v.f)
	}
	return ""
}

// Bytes returns the raw bytes of a text or blob value.
func (v Value) Bytes() []byte { return v.bytes }

// IsNull reports whether the column held NULL.
func (v Value) IsNull() bool { return v.Kind == KindNull }

// decodeRecord splits a record payload into column values.
func decodeRecord(payload []byte) ([]Value, error) {
	hdrLen, k := varint(payload)
	if k == 0 || hdrLen < int64(k) || hdrLen > int64(len(payload)) {
		return nil, fmt.Errorf("record header length %d is not within a %d byte payload", hdrLen, len(payload))
	}

	// Serial types occupy the rest of the header; values follow it.
	types := payload[k:hdrLen]
	body := payload[hdrLen:]

	var vals []Value
	at := 0
	for len(types) > 0 {
		st, n := varint(types)
		if n == 0 {
			return nil, errors.New("truncated serial type")
		}
		types = types[n:]

		size, err := serialSize(st)
		if err != nil {
			return nil, err
		}
		if at+size > len(body) {
			return nil, fmt.Errorf("column of %d bytes runs past the record body", size)
		}
		vals = append(vals, decodeValue(st, body[at:at+size]))
		at += size
	}
	return vals, nil
}

// serialSize is the byte width of a serial type.
func serialSize(st int64) (int, error) {
	switch st {
	case 0, 8, 9:
		return 0, nil
	case 1:
		return 1, nil
	case 2:
		return 2, nil
	case 3:
		return 3, nil
	case 4:
		return 4, nil
	case 5:
		return 6, nil
	case 6, 7:
		return 8, nil
	case 10, 11:
		return 0, fmt.Errorf("serial type %d is reserved for internal use", st)
	}
	if st < 12 {
		return 0, fmt.Errorf("unknown serial type %d", st)
	}
	if st%2 == 0 {
		return int((st - 12) / 2), nil
	}
	return int((st - 13) / 2), nil
}

// decodeValue builds a value from its serial type and bytes.
func decodeValue(st int64, b []byte) Value {
	switch st {
	case 0:
		return Value{Kind: KindNull}
	case 1, 2, 3, 4, 5, 6:
		return Value{Kind: KindInt, i: signedBE(b)}
	case 7:
		return Value{Kind: KindFloat, f: math.Float64frombits(binary.BigEndian.Uint64(b))}
	case 8:
		return Value{Kind: KindInt, i: 0}
	case 9:
		return Value{Kind: KindInt, i: 1}
	}
	if st%2 == 0 {
		return Value{Kind: KindBlob, bytes: b}
	}
	return Value{Kind: KindText, bytes: b}
}

// signedBE reads a big-endian two's complement integer of 1 to 8 bytes.
func signedBE(b []byte) int64 {
	if len(b) == 0 {
		return 0
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	// Sign-extend from the actual width.
	shift := uint(64 - 8*len(b))
	return int64(v<<shift) >> shift
}
