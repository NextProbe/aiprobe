# SQLite reader fixtures

These databases are written by the real `sqlite3` CLI, not by the reader under
test. That is the point: a hand-written parser checked only against its own
output proves nothing about whether it can read what SQLite actually produces.

Regenerate with:

```sh
./generate.sh
```

Requires the `sqlite3` CLI, plus `python3` for the one fixture the CLI cannot
produce. The fixtures are committed so the test suite does not depend on either
being installed.

| File | What it covers |
| --- | --- |
| `kv_small.sqlite` | The `cursorDiskKV(key, value)` shape Cursor uses, single leaf page |
| `kv_overflow.sqlite` | A 26,800-byte value over 512-byte pages: ~54 overflow pages in one chain |
| `kv_many.sqlite` | 400 rows over 512-byte pages, so the table b-tree has interior pages. Also contains index pages, which the reader must refuse rather than mistake for table pages |
| `types.sqlite` | Every serial type: NULL, the six integer widths, both integer literals, float, text, empty text, multi-byte UTF-8, blob |
| `wal.sqlite` | Declares WAL mode with no sidecar, which is what a clean shutdown leaves. It must read normally, or every closed agent is refused |
| `wal_pending.sqlite` + `-wal` | A live log: rows committed only to the sidecar, a row the log supersedes, and b-tree pages the main file never had. Both files are fixtures |
| `ddl.sqlite` | A quoted column name containing a space, plus table-level `PRIMARY KEY` and `CHECK` constraints that are not columns |

`wal_pending.sqlite` is built by Python's `sqlite3` module rather than the CLI.
The CLI checkpoints the log when it exits, which destroys the very state being
captured; a live handle is needed to copy the two files mid-transaction. It is
the same SQLite library either way, so the fixture is still SQLite's output and
not the reader's.

The damaged-log and torn-tail cases are derived from `wal_pending.sqlite-wal`
inside the tests rather than committed, since each is one byte away from the
valid file.

Page sizes are deliberately mixed. 512 bytes forces multi-level b-trees and long
overflow chains from very little data, which keeps the fixtures small while still
exercising the paths that matter.

Expected values in the tests come from `sqlite3` queries against these same
files. When capturing a digest, use `writefile()` rather than a shell pipeline:
the CLI appends a newline to query output, which makes the digest wrong by one
byte.
