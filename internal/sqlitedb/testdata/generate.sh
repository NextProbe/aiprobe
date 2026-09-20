#!/bin/bash
# Generates SQLite fixtures with the real sqlite3 CLI so the hand-written reader
# is verified against files SQLite itself produced, not against its own output.
#
# Sizes are kept deliberately small: a 512-byte page size forces multi-level
# b-trees and long overflow chains with far less data than the 4096-byte default
# would need, so the structures under test are exercised without committing
# hundreds of kilobytes of fixtures.
set -euo pipefail
OUT="${1:-$(cd "$(dirname "$0")" && pwd)}"
mkdir -p "$OUT"
# The -wal sidecars are fixtures too, so the glob has to reach them.
rm -f "$OUT"/*.sqlite "$OUT"/*.sqlite-wal "$OUT"/*.sqlite-shm

# 1. Small key/value table, the shape Cursor uses. Default page size.
sqlite3 "$OUT/kv_small.sqlite" <<'SQL'
CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);
INSERT INTO cursorDiskKV VALUES ('bubbleId:chat-1:msg-1', '{"text":"hello","type":1}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:chat-1:msg-2', '{"text":"hi there","type":2}');
INSERT INTO cursorDiskKV VALUES ('composerData:chat-1', '{"name":"My chat"}');
VACUUM;
SQL

# 2. A value spanning a long overflow chain: ~24KB over 512-byte pages is about
#    50 overflow pages, so the chain is walked many times rather than once.
python3 - "$OUT/big.txt" <<'PY'
import sys
with open(sys.argv[1], 'w') as f:
    for i in range(400):
        f.write("line %05d of a long tool result that must survive overflow paging\n" % i)
PY
sqlite3 "$OUT/kv_overflow.sqlite" <<SQL
PRAGMA page_size=512;
VACUUM;
CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);
INSERT INTO cursorDiskKV VALUES ('bubbleId:chat-1:big', readfile('$OUT/big.txt'));
INSERT INTO cursorDiskKV VALUES ('bubbleId:chat-1:small', 'tiny');
VACUUM;
SQL
rm -f "$OUT/big.txt"

# 3. Enough rows over small pages to force interior b-tree pages above the leaves.
{
  echo 'PRAGMA page_size=512;'
  echo 'VACUUM;'
  echo 'CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);'
  echo 'BEGIN;'
  for i in $(seq 1 400); do
    printf "INSERT INTO cursorDiskKV VALUES ('bubbleId:chat-1:msg-%04d', '{\"text\":\"message %d\",\"type\":1}');\n" "$i" "$i"
  done
  echo 'COMMIT;'
  echo 'VACUUM;'
} | sqlite3 "$OUT/kv_many.sqlite"

# 4. Every serial type the record format defines.
sqlite3 "$OUT/types.sqlite" <<'SQL'
CREATE TABLE vals (n TEXT, v);
INSERT INTO vals VALUES ('null', NULL);
INSERT INTO vals VALUES ('zero', 0);
INSERT INTO vals VALUES ('one', 1);
INSERT INTO vals VALUES ('i8', 100);
INSERT INTO vals VALUES ('i8neg', -100);
INSERT INTO vals VALUES ('i16', 30000);
INSERT INTO vals VALUES ('i24', 8000000);
INSERT INTO vals VALUES ('i32', 2000000000);
INSERT INTO vals VALUES ('i48', 140000000000);
INSERT INTO vals VALUES ('i64', 9000000000000000000);
INSERT INTO vals VALUES ('i64neg', -9000000000000000000);
INSERT INTO vals VALUES ('float', 3.5);
INSERT INTO vals VALUES ('floatneg', -0.125);
INSERT INTO vals VALUES ('text', 'hello world');
INSERT INTO vals VALUES ('empty', '');
INSERT INTO vals VALUES ('utf8', 'κόσμε 世界 🌍');
INSERT INTO vals VALUES ('blob', x'00010203fffe');
VACUUM;
SQL

# 5. WAL mode with no sidecar, which is what a clean shutdown leaves behind: the
#    log has been checkpointed away but the header still declares version 2. This
#    has to read like any other database.
rm -f "$OUT/wal.sqlite"*
sqlite3 "$OUT/wal.sqlite" <<'SQL'
PRAGMA journal_mode=WAL;
CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);
INSERT INTO cursorDiskKV VALUES ('k', 'v');
SQL
rm -f "$OUT/wal.sqlite-wal" "$OUT/wal.sqlite-shm"

# 5b. WAL mode with a live sidecar holding rows that are not in the main file. This
#     is the state a running Cursor leaves its store in, and the reader has to see
#     through it.
#
#     The sqlite3 CLI cannot produce this: it checkpoints the log when it exits,
#     which is exactly what has to be avoided. So the files are copied while a
#     connection is still open, which needs a language with a live handle. Python's
#     sqlite3 module is the same SQLite library, so the fixture is still written by
#     SQLite rather than by the code under test.
python3 - "$OUT/wal_pending.sqlite" <<'PY'
import os, shutil, sqlite3, sys

out = sys.argv[1]
work = os.path.join(os.path.dirname(out), "_walwork.sqlite")
for p in (work, work + "-wal", work + "-shm", out, out + "-wal", out + "-shm"):
    if os.path.exists(p):
        os.remove(p)

con = sqlite3.connect(work, isolation_level=None)
con.execute("PRAGMA page_size=512")
con.execute("PRAGMA journal_mode=WAL")
con.execute("CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)")
con.execute("INSERT INTO cursorDiskKV VALUES ('checkpointed','in the main file')")
con.execute("INSERT INTO cursorDiskKV VALUES ('superseded','old value')")
# Fold the rows above into the main file and reset the log, so that everything
# after this point is reachable only through the sidecar.
con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
con.execute("PRAGMA wal_autocheckpoint=0")

# Three separate transactions, so the log carries several commit markers.
con.execute("INSERT INTO cursorDiskKV VALUES ('logged-1','only in the log')")

con.execute("BEGIN")
con.execute("INSERT INTO cursorDiskKV VALUES ('logged-2','also only in the log')")
con.execute("UPDATE cursorDiskKV SET value='new value' WHERE key='superseded'")
con.execute("COMMIT")

# Enough rows to grow the table past its original leaf page, so the log carries a
# b-tree that gained pages rather than one page rewritten in place.
con.execute("BEGIN")
for i in range(24):
    con.execute("INSERT INTO cursorDiskKV VALUES (?,?)", ("logged-pad-%02d" % i, "padding row %d" % i))
con.execute("COMMIT")

# Copy with the connection still open: closing checkpoints the log away.
shutil.copyfile(work, out)
shutil.copyfile(work + "-wal", out + "-wal")
con.close()
for p in (work, work + "-wal", work + "-shm"):
    if os.path.exists(p):
        os.remove(p)
PY

# 6. A table-level constraint, so column-name parsing is tested against DDL that
#    is not just a list of columns.
sqlite3 "$OUT/ddl.sqlite" <<'SQL'
CREATE TABLE items (
  id INTEGER,
  "quoted name" TEXT,
  amount NUMERIC(10, 2) DEFAULT 0,
  PRIMARY KEY (id),
  CHECK (amount >= 0)
);
INSERT INTO items VALUES (1, 'a', 5);
VACUUM;
SQL

echo "--- generated ---"
total=0
for f in "$OUT"/*.sqlite; do
  sz=$(wc -c < "$f" | tr -d ' ')
  total=$((total + sz))
  printf '%-22s %7s bytes  page_size=%s\n' "$(basename "$f")" "$sz" "$(sqlite3 "$f" 'PRAGMA page_size;')"
done
echo "total: $total bytes"
