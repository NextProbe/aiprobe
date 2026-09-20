#!/bin/bash
# Builds Cursor-shaped chat databases with the sqlite3 CLI.
#
# Cursor was not installed on the machine where this adapter was written, so these
# fixtures encode the schema as documented by Cursor's own store layout and by
# several independent community tools: a cursorDiskKV(key, value) table holding
# composerData:<id> and bubbleId:<id>:<bubbleId> JSON, with type 1 for the user and
# 2 for the assistant.
#
# They verify the adapter's behaviour against that schema. They do not verify the
# schema itself, which is why the adapter warns about anything it reads on a guess.
set -euo pipefail
OUT="${1:-$(cd "$(dirname "$0")" && pwd)}"
mkdir -p "$OUT"
# The -wal sidecar is a fixture too, so the glob has to reach it.
rm -f "$OUT"/*.vscdb "$OUT"/*.vscdb-wal "$OUT"/*.vscdb-shm

# A workspace database with two conversations, tool calls and attached context.
sqlite3 "$OUT/workspace.vscdb" <<'SQL'
CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB);
CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);

INSERT INTO ItemTable VALUES ('workbench.explorer.views.state', '{}');

-- Conversation one: the larger of the two, so it is the default choice.
INSERT INTO cursorDiskKV VALUES ('composerData:aaaa-1111',
  '{"composerId":"aaaa-1111","name":"Refactor the auth module","createdAt":1775200000000,"lastUpdatedAt":1775206000000,"unifiedMode":"agent"}');

INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b01',
  '{"type":1,"bubbleId":"b01","text":"Refactor the auth module to use the new token store.",
    "attachedCodeChunks":[{"relativeWorkspacePath":"src/auth.go","text":"package auth\n\nfunc Login() error { return nil }\n"}]}');

INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b02',
  '{"type":2,"bubbleId":"b02","text":"I will read the file first.","usageUuid":"u-1",
    "timingInfo":{"clientStartTime":1775200001000,"clientEndTime":1775200004000},
    "thinking":{"text":"The token store lives in internal/tokens."},
    "toolFormerData":{"name":"read_file","status":"completed",
      "params":{"target_file":"src/auth.go"},
      "result":"package auth\n\nfunc Login() error { return nil }\n"}}');

INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b03',
  '{"type":1,"bubbleId":"b03","text":"Yes, go ahead."}');

INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b04',
  '{"type":2,"bubbleId":"b04","text":"Done. I edited the file.","usageUuid":"u-2",
    "timingInfo":{"clientStartTime":1775200010000,"clientSettleTime":1775200014000},
    "toolFormerData":{"name":"search_replace","status":"completed",
      "params":{"target_file":"src/auth.go"},"result":"applied"}}');

-- An MCP tool call, so name-based origin detection is covered.
INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b05',
  '{"type":2,"bubbleId":"b05","text":"Checking the tracker.","usageUuid":"u-3",
    "toolFormerData":{"name":"mcp_github_create_issue","status":"completed",
      "params":{"title":"Refactor auth"},"result":"issue 42 created"}}');

-- A tool record with no recoverable name, which must not become a nameless tool.
INSERT INTO cursorDiskKV VALUES ('bubbleId:aaaa-1111:b06',
  '{"type":2,"bubbleId":"b06","text":"One more step.",
    "toolFormerData":{"unexpected":"shape","payload":"some text that still cost tokens"}}');

-- Conversation two: smaller, so it is only reachable by naming its id.
INSERT INTO cursorDiskKV VALUES ('composerData:bbbb-2222',
  '{"composerId":"bbbb-2222","name":"Quick question","createdAt":1775300000000,"lastUpdatedAt":1775300500000}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:bbbb-2222:c01',
  '{"type":1,"bubbleId":"c01","text":"What does this regex do?"}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:bbbb-2222:c02',
  '{"type":2,"bubbleId":"c02","text":"It matches a semantic version.","usageUuid":"v-1",
    "timingInfo":{"clientStartTime":1775300001000,"clientEndTime":1775300002000}}');
VACUUM;
SQL

# A global-storage style database with one conversation and no workspace.
sqlite3 "$OUT/global.vscdb" <<'SQL'
CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB);
CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB);
INSERT INTO cursorDiskKV VALUES ('composerData:cccc-3333',
  '{"composerId":"cccc-3333","name":"Scratch","createdAt":1775400000000,"lastUpdatedAt":1775400900000}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:cccc-3333:d01', '{"type":1,"bubbleId":"d01","text":"hello"}');
INSERT INTO cursorDiskKV VALUES ('bubbleId:cccc-3333:d02', '{"type":2,"bubbleId":"d02","text":"Hi there."}');
VACUUM;
SQL

# VS Code style storage with no chat table, so the wrong-file message is testable.
sqlite3 "$OUT/notcursor.vscdb" <<'SQL'
CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB);
INSERT INTO ItemTable VALUES ('someEditorState', '{}');
VACUUM;
SQL

# A store in the state a running Cursor leaves it in: WAL mode, with the whole
# conversation still in the -wal sidecar and nothing in the main file's chat table.
# This is the normal case on a machine with Cursor installed, not an edge case.
#
# The sqlite3 CLI cannot build it, because it checkpoints the log on exit and that
# is the state being captured. Python's sqlite3 module is the same SQLite library
# and can copy the files while a connection is still open.
python3 - "$OUT/wal.vscdb" <<'PY'
import os, shutil, sqlite3, sys

out = sys.argv[1]
work = os.path.join(os.path.dirname(out), "_walwork.vscdb")
for p in (work, work + "-wal", work + "-shm", out, out + "-wal", out + "-shm"):
    if os.path.exists(p):
        os.remove(p)

con = sqlite3.connect(work, isolation_level=None)
con.execute("PRAGMA page_size=512")
con.execute("PRAGMA journal_mode=WAL")
con.execute("CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value BLOB)")
con.execute("CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value BLOB)")
con.execute("INSERT INTO ItemTable VALUES ('someEditorState','{}')")
# Fold the empty chat table into the main file and reset the log, so the
# conversation below is reachable only through the sidecar.
con.execute("PRAGMA wal_checkpoint(TRUNCATE)")
con.execute("PRAGMA wal_autocheckpoint=0")

con.execute("BEGIN")
con.execute(
    "INSERT INTO cursorDiskKV VALUES ('composerData:wal-9999',?)",
    ('{"composerId":"wal-9999","name":"Only in the log",'
     '"createdAt":1775500000000,"lastUpdatedAt":1775500600000,"unifiedMode":"agent"}',),
)
con.execute(
    "INSERT INTO cursorDiskKV VALUES ('bubbleId:wal-9999:e01',?)",
    ('{"type":1,"bubbleId":"e01","text":"Does the write-ahead log get read?"}',),
)
con.execute(
    "INSERT INTO cursorDiskKV VALUES ('bubbleId:wal-9999:e02',?)",
    ('{"type":2,"bubbleId":"e02","text":"It does now.","usageUuid":"w-1",'
     '"timingInfo":{"clientStartTime":1775500001000,"clientEndTime":1775500003000}}',),
)
con.execute("COMMIT")

# Copy with the connection still open: closing checkpoints the log away.
shutil.copyfile(work, out)
shutil.copyfile(work + "-wal", out + "-wal")
con.close()
for p in (work, work + "-wal", work + "-shm"):
    if os.path.exists(p):
        os.remove(p)
PY

echo "--- generated ---"
for f in "$OUT"/*.vscdb "$OUT"/*.vscdb-wal; do
  printf '%-20s %6s bytes\n' "$(basename "$f")" "$(wc -c < "$f" | tr -d ' ')"
done
