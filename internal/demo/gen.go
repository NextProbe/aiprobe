//go:build ignore

// Command gen writes the synthetic Claude Code session that backs
// `aiprobe demo`.
//
// Run it from the repository root after changing the scenario:
//
//	go run internal/demo/gen.go
//
// The generator is fully deterministic: fixed timestamps, fixed ids, fixed
// filler text. Regenerating it without changing this file produces a
// byte-identical transcript, which keeps the demo report stable and makes the
// numbers in README.md reproducible.
//
// The scenario is a plausible bad session, built to exercise every analyzer:
//
//   - 6 MCP servers with 41 tools loaded, of which 2 are ever called
//   - a large CLAUDE.md-style rules block re-injected as a system reminder
//   - the same three files read over and over, always in full
//   - a verbose test command run five times with near-identical output
//   - an MCP tool that returns the same 3k payload four times
//   - an idle gap past the 5 minute cache TTL
//   - a mid-session model switch
//   - a rules edit that rewrites the prefix
//   - one cache miss with no local explanation, so "unknown" is exercised
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/NextProbe/aiprobe/internal/tokenize"
)

// systemPromptTokens is the assumed size of the agent's own system prompt,
// which no transcript contains. Claude Code's is in this range once tool
// instructions and environment details are included.
const systemPromptTokens = 11800

const (
	sessionID = "9f3c71d2-4e8a-4c15-b7a0-2d6ef1a90b34"
	cwd       = "/Users/dev/code/orders-api"
	branch    = "feat/payments"
	version   = "2.0.29"
	modelMain = "claude-sonnet-4-5-20250929"
	modelAlt  = "claude-opus-4-1-20250805"
)

var start = time.Date(2026, 3, 12, 9, 14, 3, 0, time.UTC)

// cacheMode says what the prompt cache did for a request.
type cacheMode int

const (
	// cacheWarm reuses the whole previous prompt.
	cacheWarm cacheMode = iota
	// cacheCold reuses nothing: expiry, model switch or an invalidated prefix.
	cacheCold
	// cachePartial reuses a fraction of the previous prompt.
	cachePartial
)

type writer struct {
	buf      bytes.Buffer
	seq      int
	now      time.Time
	lastUUID string

	// visible tracks the estimated token size of the conversation so far, using
	// the same estimator AIProbe uses. Usage counters are derived from it,
	// so the synthetic session is internally consistent: the demo report's
	// calibration factor lands near 1.0 and its attribution is trustworthy
	// rather than an artifact of hand-written numbers.
	visible    int
	prevPrompt int
	requests   int
	// overhead is the invisible fixed cost of every request: the system prompt
	// plus the tool schema block.
	overhead int
}

// addVisible accounts for content entering the conversation.
func (w *writer) addVisible(text string) { w.visible += tokenize.Estimate(text) }

func (w *writer) uuid() string {
	w.seq++
	return fmt.Sprintf("11111111-2222-4333-8444-%012d", w.seq)
}

func (w *writer) advance(d time.Duration) { w.now = w.now.Add(d) }

func (w *writer) ts() string { return w.now.UTC().Format("2006-01-02T15:04:05.000Z") }

func (w *writer) emit(rec map[string]any) {
	b, err := json.Marshal(rec)
	if err != nil {
		panic(err)
	}
	w.buf.Write(b)
	w.buf.WriteByte('\n')
}

func (w *writer) base(kind string) map[string]any {
	id := w.uuid()
	rec := map[string]any{
		"type":        kind,
		"uuid":        id,
		"parentUuid":  w.lastUUID,
		"isSidechain": false,
		"userType":    "external",
		"cwd":         cwd,
		"sessionId":   sessionID,
		"version":     version,
		"gitBranch":   branch,
		"timestamp":   w.ts(),
	}
	if w.lastUUID == "" {
		rec["parentUuid"] = nil
	}
	w.lastUUID = id
	return rec
}

// user writes a plain user turn.
func (w *writer) user(text string) {
	rec := w.base("user")
	rec["message"] = map[string]any{"role": "user", "content": text}
	w.emit(rec)
	w.addVisible(text)
}

// userReminder writes a user turn carrying an injected rules block.
func (w *writer) userReminder(text, reminder string) {
	full := text + "\n<system-reminder>\n" + reminder + "\n</system-reminder>"
	rec := w.base("user")
	rec["message"] = map[string]any{"role": "user", "content": full}
	w.emit(rec)
	w.addVisible(full)
}

type toolCall struct {
	id    string
	name  string
	input map[string]any
}

// assistant writes an assistant turn with optional tool calls, deriving the
// provider usage counters from the conversation built so far.
func (w *writer) assistant(model, text string, mode cacheMode, frac float64, calls []toolCall) {
	prompt := w.overhead + w.visible

	cacheRead := 0
	switch mode {
	case cacheWarm:
		if w.requests > 0 {
			cacheRead = w.prevPrompt
		}
	case cachePartial:
		if w.requests > 0 {
			cacheRead = int(float64(w.prevPrompt) * frac)
		}
	case cacheCold:
		cacheRead = 0
	}
	if cacheRead > prompt {
		cacheRead = prompt
	}
	// Claude Code puts a cache breakpoint at the end of the prompt, so the
	// untouched tail is billed as fresh input and the rest is written to cache.
	input := 4 + w.requests
	cacheWrite := prompt - cacheRead - input
	if cacheWrite < 0 {
		cacheWrite = 0
		input = prompt - cacheRead
	}
	output := tokenize.Estimate(text) + 60

	content := []any{}
	if text != "" {
		content = append(content, map[string]any{"type": "text", "text": text})
	}
	for _, c := range calls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    c.id,
			"name":  c.name,
			"input": c.input,
		})
	}
	rec := w.base("assistant")
	rec["requestId"] = fmt.Sprintf("req_%04d%s", w.seq, "abcdef")
	stop := "end_turn"
	if len(calls) > 0 {
		stop = "tool_use"
	}
	rec["message"] = map[string]any{
		"id":            fmt.Sprintf("msg_%04daiprobe", w.seq),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stop,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                input,
			"cache_creation_input_tokens": cacheWrite,
			"cache_read_input_tokens":     cacheRead,
			"cache_creation": map[string]any{
				"ephemeral_5m_input_tokens": cacheWrite,
				"ephemeral_1h_input_tokens": 0,
			},
			"output_tokens": output,
			"service_tier":  "standard",
		},
	}
	w.emit(rec)

	w.prevPrompt = prompt
	w.requests++

	// The assistant's own reply and its tool_use blocks join the conversation.
	w.addVisible(text)
	for _, c := range calls {
		args, err := json.Marshal(c.input)
		if err != nil {
			panic(err)
		}
		w.visible += tokenize.Estimate(string(args)) + tokenize.Estimate(c.name)
	}
}

// toolResult writes the user-side result record for a tool call.
func (w *writer) toolResult(id, content string, payload map[string]any) {
	rec := w.base("user")
	rec["message"] = map[string]any{
		"role": "user",
		"content": []any{map[string]any{
			"type":        "tool_result",
			"tool_use_id": id,
			"content":     content,
			"is_error":    false,
		}},
	}
	if payload != nil {
		rec["toolUseResult"] = payload
	}
	w.emit(rec)
	w.addVisible(content)
}

func (w *writer) summary(text string) {
	w.emit(map[string]any{"type": "summary", "summary": text, "leafUuid": nil})
}

// ---------- synthetic content ----------

// goFile builds a deterministic Go-like file body of roughly n lines.
func goFile(pkg, name string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "package %s\n\nimport (\n\t\"context\"\n\t\"errors\"\n\t\"fmt\"\n\t\"time\"\n)\n\n", pkg)
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "// %s%02d validates and persists one %s record.\n", name, i, name)
		fmt.Fprintf(&b, "func %s%02d(ctx context.Context, id string, amount int64) (*%s, error) {\n", name, i, name)
		fmt.Fprintf(&b, "\tif id == \"\" {\n\t\treturn nil, errors.New(\"%s: empty id\")\n\t}\n", name)
		fmt.Fprintf(&b, "\tif amount <= 0 {\n\t\treturn nil, fmt.Errorf(\"%s %%s: amount must be positive, got %%d\", id, amount)\n\t}\n", name)
		fmt.Fprintf(&b, "\trec := &%s{ID: id, Amount: amount, CreatedAt: time.Now().UTC()}\n", name)
		fmt.Fprintf(&b, "\tif err := rec.validate(ctx); err != nil {\n\t\treturn nil, fmt.Errorf(\"validate %s %%s: %%w\", id, err)\n\t}\n", name)
		b.WriteString("\treturn rec, nil\n}\n\n")
	}
	return b.String()
}

// testOutput builds verbose go test output; seed changes only the timings, so
// consecutive runs are near-identical rather than identical.
func testOutput(seed int, pkgs int) string {
	var b strings.Builder
	b.WriteString("=== RUN   TestSuite\n")
	for i := 0; i < pkgs; i++ {
		fmt.Fprintf(&b, "=== RUN   TestSuite/case_%02d\n", i)
		fmt.Fprintf(&b, "    handler_test.go:%d: request accepted, order=ord_%05d status=pending\n", 40+i, 1000+i)
		fmt.Fprintf(&b, "    handler_test.go:%d: payment authorized, provider=stripe latency=%dms\n", 41+i, 120+i+seed)
		fmt.Fprintf(&b, "--- PASS: TestSuite/case_%02d (0.%02ds)\n", i, 3+i%7)
	}
	fmt.Fprintf(&b, "--- PASS: TestSuite (%d.45s)\n", 12+seed)
	b.WriteString("PASS\nok  \tgithub.com/dev/orders-api/internal/http\t")
	fmt.Fprintf(&b, "%d.451s\n", 12+seed)
	return b.String()
}

// mcpPayload builds a repeated MCP response body.
func mcpPayload(n int) string {
	var b strings.Builder
	b.WriteString("{\n  \"issues\": [\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "    {\"key\": \"ORD-%d\", \"summary\": \"Payment retry loop on declined card\", \"status\": \"In Progress\", \"assignee\": \"dev@example.com\", \"labels\": [\"payments\", \"backend\", \"p1\"], \"updated\": \"2026-03-10T11:2%d:00Z\", \"description\": \"When the provider returns a soft decline the worker retries immediately instead of backing off, which trips the provider rate limit and produces duplicate authorizations.\"}%s\n", 400+i, i%10, comma(i, n))
	}
	b.WriteString("  ]\n}\n")
	return b.String()
}

func comma(i, n int) string {
	if i == n-1 {
		return ""
	}
	return ","
}

const rulesBlock = `# Project rules (orders-api)

## Architecture
- HTTP handlers live in internal/http and must stay free of business logic.
- Business logic lives in internal/orders and internal/payments.
- Every exported function takes a context.Context as its first argument.
- Errors are wrapped with fmt.Errorf and %w, never with errors.New at call sites.

## Testing
- Table-driven tests only. One assertion helper per package.
- Run go test ./... before every commit. Integration tests need docker compose up -d.
- Never skip a failing test; fix it or mark it with a linked issue.

## Style
- gofmt with tabs. Line length is not enforced but keep it readable.
- Exported identifiers carry doc comments starting with the identifier name.
- No third-party assertion libraries. Standard library testing only.
- Database access goes through the repository interfaces, never database/sql directly.

## Payments domain
- Amounts are int64 minor units. Never float.
- Currency is always an ISO-4217 string, uppercase, validated at the edge.
- Soft declines retry with exponential backoff starting at 2s, max 5 attempts.
- Every provider call is idempotent and carries an idempotency key.

## Review checklist
- Does this change touch the payments ledger? If so, require a second reviewer.
- Are new endpoints covered by contract tests?
- Does the migration have a tested rollback?`

func main() {
	// The inventory is written first because its total schema cost is part of
	// every request's fixed overhead, and the session's usage counters are
	// derived from that overhead.
	schemaTokens := writeInventory()

	w := &writer{now: start, overhead: systemPromptTokens + schemaTokens}
	w.summary("Debug duplicate payment authorizations in orders-api")

	handler := goFile("http", "Handler", 42)
	payments := goFile("payments", "Authorization", 34)
	orders := goFile("orders", "Order", 26)

	// Turn 1: opening request. The prefix is created here: system prompt, the
	// full tool schema block for 6 MCP servers, and the rules reminder.
	w.userReminder("The payments worker is creating duplicate authorizations. Find out why.", rulesBlock)
	w.assistant(modelMain, "Let me start by reading the payment worker and the handler.",
		cacheWarm, 0,
		[]toolCall{
			{"toolu_0001", "Read", map[string]any{"file_path": cwd + "/internal/payments/authorize.go"}},
			{"toolu_0002", "Read", map[string]any{"file_path": cwd + "/internal/http/handler.go"}},
		})
	w.advance(3 * time.Second)
	w.toolResult("toolu_0001", payments, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/payments/authorize.go", "content": payments, "numLines": 320},
	})
	w.toolResult("toolu_0002", handler, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/http/handler.go", "content": handler, "numLines": 396},
	})

	// Turn 2: MCP call that returns a large payload.
	w.advance(6 * time.Second)
	jira := mcpPayload(14)
	w.assistant(modelMain, "Checking the tracker for related reports.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0003", "mcp__jira__search_issues", map[string]any{"jql": "project = ORD AND labels = payments ORDER BY updated DESC", "maxResults": 14}}})
	w.advance(4 * time.Second)
	w.toolResult("toolu_0003", jira, nil)

	// Turn 3: verbose test run.
	w.advance(8 * time.Second)
	w.assistant(modelMain, "Running the suite to see the failure.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0004", "Bash", map[string]any{"command": "go test ./... -v", "description": "Run all tests verbosely"}}})
	w.advance(31 * time.Second)
	w.toolResult("toolu_0004", testOutput(0, 22), map[string]any{"stdout": testOutput(0, 22), "stderr": "", "interrupted": false, "isImage": false})

	// Turn 4: re-read the same files instead of using what is already present.
	w.advance(5 * time.Second)
	w.assistant(modelMain, "Let me look at the retry path again.",
		cacheWarm, 0,
		[]toolCall{
			{"toolu_0005", "Read", map[string]any{"file_path": cwd + "/internal/payments/authorize.go"}},
			{"toolu_0006", "Read", map[string]any{"file_path": cwd + "/internal/orders/order.go"}},
		})
	w.advance(3 * time.Second)
	w.toolResult("toolu_0005", payments, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/payments/authorize.go", "content": payments, "numLines": 320},
	})
	w.toolResult("toolu_0006", orders, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/orders/order.go", "content": orders, "numLines": 248},
	})

	// Turn 5: same MCP query again, identical payload.
	w.advance(7 * time.Second)
	w.assistant(modelMain, "Re-checking the tracker for anything I missed.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0007", "mcp__jira__search_issues", map[string]any{"jql": "project = ORD AND labels = payments ORDER BY updated DESC", "maxResults": 14}}})
	w.advance(4 * time.Second)
	w.toolResult("toolu_0007", jira, nil)

	// Turn 6: tests again, near-identical output.
	w.advance(6 * time.Second)
	w.assistant(modelMain, "Running the suite again after the change.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0008", "Bash", map[string]any{"command": "go test ./... -v", "description": "Run all tests verbosely"}}})
	w.advance(29 * time.Second)
	w.toolResult("toolu_0008", testOutput(1, 22), map[string]any{"stdout": testOutput(1, 22), "stderr": "", "interrupted": false, "isImage": false})

	// Turn 7: idle gap past the 5 minute TTL. The cached prefix is gone.
	w.advance(23 * time.Minute)
	w.user("Back. What did you find?")
	w.assistant(modelMain, "Summarizing what the retry path does.",
		cacheCold, 0,
		nil)

	// Turn 8: model switch, prefix invalidated again.
	w.advance(12 * time.Second)
	w.user("Try the reasoning model on this.")
	w.assistant(modelAlt, "Looking at the concurrency around the idempotency key.",
		cacheCold, 0,
		[]toolCall{{"toolu_0009", "Read", map[string]any{"file_path": cwd + "/internal/payments/authorize.go"}}})
	w.advance(3 * time.Second)
	w.toolResult("toolu_0009", payments, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/payments/authorize.go", "content": payments, "numLines": 320},
	})

	// Turn 9: back to the main model, rules block edited mid-session so the
	// prefix is rewritten rather than extended.
	w.advance(9 * time.Second)
	w.userReminder("I tightened the retry rule, re-read it.",
		strings.Replace(rulesBlock, "starting at 2s, max 5 attempts", "starting at 5s, max 3 attempts, guarded by a distributed lock", 1))
	w.assistant(modelMain, "Applying the backoff change with the lock.",
		cacheCold, 0,
		[]toolCall{{"toolu_0010", "Bash", map[string]any{"command": "go test ./... -v", "description": "Run all tests verbosely"}}})
	w.advance(28 * time.Second)
	w.toolResult("toolu_0010", testOutput(2, 22), map[string]any{"stdout": testOutput(2, 22), "stderr": "", "interrupted": false, "isImage": false})

	// Turn 10: MCP payload a third time.
	w.advance(7 * time.Second)
	w.assistant(modelMain, "Cross-checking the tracker once more.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0011", "mcp__jira__search_issues", map[string]any{"jql": "project = ORD AND labels = payments ORDER BY updated DESC", "maxResults": 14}}})
	w.advance(4 * time.Second)
	w.toolResult("toolu_0011", jira, nil)

	// Turn 11: a cache miss with no visible cause, so the analyzer must report
	// unknown instead of inventing one.
	w.advance(24 * time.Second)
	w.assistant(modelMain, "Checking the migration for the lock table.",
		cachePartial, 0.55,
		[]toolCall{{"toolu_0012", "Grep", map[string]any{"pattern": "idempotency_key", "path": cwd, "output_mode": "content"}}})
	w.advance(3 * time.Second)
	w.toolResult("toolu_0012", grepOutput(), nil)

	// Turn 12: repeated file reads continue, plus a repeated shell command.
	w.advance(11 * time.Second)
	w.assistant(modelMain, "Re-reading the handler before I touch it.",
		cacheWarm, 0,
		[]toolCall{
			{"toolu_0013", "Read", map[string]any{"file_path": cwd + "/internal/http/handler.go"}},
			{"toolu_0014", "Bash", map[string]any{"command": "git status --porcelain", "description": "Check working tree"}},
		})
	w.advance(3 * time.Second)
	w.toolResult("toolu_0013", handler, map[string]any{
		"type": "text",
		"file": map[string]any{"filePath": cwd + "/internal/http/handler.go", "content": handler, "numLines": 396},
	})
	w.toolResult("toolu_0014", " M internal/payments/authorize.go\n M internal/http/handler.go\n", map[string]any{"stdout": " M internal/payments/authorize.go\n M internal/http/handler.go\n", "stderr": "", "interrupted": false, "isImage": false})

	// Turn 13: fourth identical MCP payload and a fourth verbose test run.
	w.advance(8 * time.Second)
	w.assistant(modelMain, "Final check across tracker and tests.",
		cacheWarm, 0,
		[]toolCall{
			{"toolu_0015", "mcp__jira__search_issues", map[string]any{"jql": "project = ORD AND labels = payments ORDER BY updated DESC", "maxResults": 14}},
			{"toolu_0016", "Bash", map[string]any{"command": "go test ./... -v", "description": "Run all tests verbosely"}},
		})
	w.advance(30 * time.Second)
	w.toolResult("toolu_0015", jira, nil)
	w.toolResult("toolu_0016", testOutput(3, 22), map[string]any{"stdout": testOutput(3, 22), "stderr": "", "interrupted": false, "isImage": false})

	// Turn 14: one more MCP server actually used, then wrap up.
	w.advance(9 * time.Second)
	w.assistant(modelMain, "Opening a PR with the fix.",
		cacheWarm, 0,
		[]toolCall{{"toolu_0017", "mcp__github__create_pull_request", map[string]any{
			"owner": "dev", "repo": "orders-api", "title": "payments: back off and lock before retrying a soft decline",
			"head": branch, "base": "main",
			"body": "Soft declines retried immediately, tripping the provider rate limit and producing duplicate authorizations.\n\nThis change moves the retry behind a distributed lock and starts the backoff at 5s with a maximum of 3 attempts.",
		}}})
	w.advance(5 * time.Second)
	w.toolResult("toolu_0017", "{\"number\": 482, \"url\": \"https://github.example.com/dev/orders-api/pull/482\", \"state\": \"open\"}", nil)

	w.advance(6 * time.Second)
	w.assistant(modelMain, "Opened PR #482. The duplicate authorizations came from an immediate retry on soft decline with no lock; the retry now backs off from 5s and holds a lock per idempotency key.",
		cacheWarm, 0,
		nil)

	out := filepath.Join("internal", "demo", "testdata", "demo-session.jsonl")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(out, w.buf.Bytes(), 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d bytes, %d lines)\n", out, w.buf.Len(), bytes.Count(w.buf.Bytes(), []byte("\n")))
	fmt.Printf("session: %d requests, fixed overhead %d tokens/request (system %d + schemas %d)\n",
		w.requests, w.overhead, systemPromptTokens, schemaTokens)
}

func grepOutput() string {
	var b strings.Builder
	for i := 0; i < 26; i++ {
		fmt.Fprintf(&b, "internal/payments/authorize.go:%d:\tif rec.IdempotencyKey == \"\" { rec.IdempotencyKey = newKey(id) }\n", 40+i*7)
		fmt.Fprintf(&b, "internal/payments/store.go:%d:\tconst q = `INSERT INTO authorizations (id, idempotency_key, amount) VALUES ($1,$2,$3)`\n", 12+i*5)
	}
	return b.String()
}

// ---------- MCP tool inventory ----------
//
// The demo ships an inventory so the MCP Tool Tax section shows exact numbers
// rather than "unknown". Six servers expose 41 tools; the session calls two of
// them. That ratio is the point of the section.

type invToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func prop(t, desc string) map[string]any {
	return map[string]any{"type": t, "description": desc}
}

func schema(required []string, props map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"properties":           props,
		"required":             required,
		"additionalProperties": false,
	}
}

func tool(name, desc string, required []string, props map[string]any) invToolDef {
	return invToolDef{Name: name, Description: desc, InputSchema: schema(required, props)}
}

// writeInventory emits the tool inventory and returns its total estimated
// schema cost, computed exactly the way internal/analyzer/mcp computes it.
func writeInventory() int {
	servers := map[string][]invToolDef{}

	servers["jira"] = []invToolDef{
		tool("search_issues", "Search issues with a JQL query and return matching issues with their fields, labels, assignee and description.",
			[]string{"jql"}, map[string]any{
				"jql":        prop("string", "JQL query string, for example: project = ORD AND status != Done"),
				"maxResults": prop("integer", "Maximum number of issues to return, between 1 and 100."),
				"fields":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Field names to include in the response."},
				"expand":     prop("string", "Comma separated list of entities to expand, such as changelog or renderedFields."),
			}),
		tool("get_issue", "Fetch a single issue by key, including comments, attachments and the full change history.",
			[]string{"issueKey"}, map[string]any{
				"issueKey": prop("string", "Issue key such as ORD-412."),
				"expand":   prop("string", "Entities to expand in the response."),
			}),
		tool("create_issue", "Create a new issue in a project with a summary, description, type and optional labels and assignee.",
			[]string{"project", "summary", "issueType"}, map[string]any{
				"project":     prop("string", "Project key the issue belongs to."),
				"summary":     prop("string", "One line summary of the issue."),
				"description": prop("string", "Full issue description in wiki markup."),
				"issueType":   prop("string", "Issue type name, for example Bug, Task or Story."),
				"labels":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels to attach."},
				"assignee":    prop("string", "Account id of the assignee."),
				"priority":    prop("string", "Priority name such as Highest, High, Medium, Low."),
			}),
		tool("update_issue", "Update fields on an existing issue, such as summary, description, labels, priority or custom fields.",
			[]string{"issueKey", "fields"}, map[string]any{
				"issueKey": prop("string", "Issue key to update."),
				"fields":   map[string]any{"type": "object", "description": "Map of field name to new value."},
				"notify":   prop("boolean", "Whether watchers should be notified of the change."),
			}),
		tool("add_comment", "Add a comment to an issue, optionally restricted to a project role or group.",
			[]string{"issueKey", "body"}, map[string]any{
				"issueKey":   prop("string", "Issue key to comment on."),
				"body":       prop("string", "Comment body in wiki markup."),
				"visibility": prop("string", "Role or group name the comment is restricted to."),
			}),
		tool("transition_issue", "Move an issue through a workflow transition, optionally setting a resolution.",
			[]string{"issueKey", "transition"}, map[string]any{
				"issueKey":   prop("string", "Issue key to transition."),
				"transition": prop("string", "Transition name or id."),
				"resolution": prop("string", "Resolution to set when the transition closes the issue."),
				"comment":    prop("string", "Comment to add alongside the transition."),
			}),
		tool("list_projects", "List all projects visible to the authenticated user, with keys, names and lead.",
			nil, map[string]any{
				"expand":     prop("string", "Entities to expand."),
				"maxResults": prop("integer", "Page size."),
			}),
		tool("list_sprints", "List sprints for a board, including state, start and end dates.",
			[]string{"boardId"}, map[string]any{
				"boardId": prop("integer", "Numeric board id."),
				"state":   prop("string", "Filter by sprint state: active, future or closed."),
			}),
		tool("get_board", "Fetch a board configuration including columns, filter and estimation settings.",
			[]string{"boardId"}, map[string]any{"boardId": prop("integer", "Numeric board id.")}),
		tool("assign_issue", "Assign an issue to a user, or unassign it by passing an empty account id.",
			[]string{"issueKey"}, map[string]any{
				"issueKey":  prop("string", "Issue key to assign."),
				"accountId": prop("string", "Account id of the new assignee, empty to unassign."),
			}),
		tool("link_issues", "Create a link between two issues, such as blocks, relates to or duplicates.",
			[]string{"inwardIssue", "outwardIssue", "linkType"}, map[string]any{
				"inwardIssue":  prop("string", "Key of the inward issue."),
				"outwardIssue": prop("string", "Key of the outward issue."),
				"linkType":     prop("string", "Link type name."),
				"comment":      prop("string", "Comment to attach to the link."),
			}),
		tool("search_users", "Search users by display name or email and return account ids.",
			[]string{"query"}, map[string]any{
				"query":      prop("string", "Partial display name or email."),
				"maxResults": prop("integer", "Page size."),
			}),
	}

	servers["github"] = []invToolDef{
		tool("create_pull_request", "Open a pull request from a head branch into a base branch with a title and body.",
			[]string{"owner", "repo", "title", "head", "base"}, map[string]any{
				"owner": prop("string", "Repository owner login."),
				"repo":  prop("string", "Repository name."),
				"title": prop("string", "Pull request title."),
				"head":  prop("string", "Branch containing the changes."),
				"base":  prop("string", "Branch the changes are merged into."),
				"body":  prop("string", "Pull request description in Markdown."),
				"draft": prop("boolean", "Open the pull request as a draft."),
			}),
		tool("list_pull_requests", "List pull requests in a repository filtered by state, base branch or author.",
			[]string{"owner", "repo"}, map[string]any{
				"owner": prop("string", "Repository owner login."),
				"repo":  prop("string", "Repository name."),
				"state": prop("string", "Filter by state: open, closed or all."),
				"base":  prop("string", "Filter by base branch."),
				"sort":  prop("string", "Sort field: created, updated, popularity."),
			}),
		tool("get_pull_request_diff", "Fetch the unified diff of a pull request, optionally limited to specific files.",
			[]string{"owner", "repo", "pullNumber"}, map[string]any{
				"owner":      prop("string", "Repository owner login."),
				"repo":       prop("string", "Repository name."),
				"pullNumber": prop("integer", "Pull request number."),
				"paths":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Restrict the diff to these paths."},
			}),
		tool("create_issue_comment", "Add a comment to an issue or pull request.",
			[]string{"owner", "repo", "issueNumber", "body"}, map[string]any{
				"owner":       prop("string", "Repository owner login."),
				"repo":        prop("string", "Repository name."),
				"issueNumber": prop("integer", "Issue or pull request number."),
				"body":        prop("string", "Comment body in Markdown."),
			}),
		tool("list_workflow_runs", "List GitHub Actions workflow runs with status, conclusion and timing.",
			[]string{"owner", "repo"}, map[string]any{
				"owner":    prop("string", "Repository owner login."),
				"repo":     prop("string", "Repository name."),
				"workflow": prop("string", "Workflow file name or id."),
				"status":   prop("string", "Filter by status such as completed or in_progress."),
				"branch":   prop("string", "Filter by branch."),
			}),
		tool("get_workflow_run_logs", "Download the logs of a workflow run as text, optionally for one job.",
			[]string{"owner", "repo", "runId"}, map[string]any{
				"owner": prop("string", "Repository owner login."),
				"repo":  prop("string", "Repository name."),
				"runId": prop("integer", "Workflow run id."),
				"jobId": prop("integer", "Restrict to a single job id."),
			}),
		tool("search_code", "Search code across repositories with GitHub's code search syntax.",
			[]string{"query"}, map[string]any{
				"query":   prop("string", "Search query using GitHub code search qualifiers."),
				"perPage": prop("integer", "Results per page."),
			}),
		tool("create_branch", "Create a branch from a source ref.",
			[]string{"owner", "repo", "branch", "fromRef"}, map[string]any{
				"owner":   prop("string", "Repository owner login."),
				"repo":    prop("string", "Repository name."),
				"branch":  prop("string", "New branch name."),
				"fromRef": prop("string", "Ref to branch from."),
			}),
		tool("merge_pull_request", "Merge a pull request using merge, squash or rebase.",
			[]string{"owner", "repo", "pullNumber"}, map[string]any{
				"owner":       prop("string", "Repository owner login."),
				"repo":        prop("string", "Repository name."),
				"pullNumber":  prop("integer", "Pull request number."),
				"mergeMethod": prop("string", "One of merge, squash or rebase."),
				"commitTitle": prop("string", "Title for the merge commit."),
			}),
		tool("list_releases", "List releases with tags, names and publication dates.",
			[]string{"owner", "repo"}, map[string]any{
				"owner":   prop("string", "Repository owner login."),
				"repo":    prop("string", "Repository name."),
				"perPage": prop("integer", "Results per page."),
			}),
	}

	servers["postgres"] = []invToolDef{
		tool("query", "Run a read-only SQL query and return rows as JSON. Statements that write are rejected.",
			[]string{"sql"}, map[string]any{
				"sql":    prop("string", "SELECT statement to execute."),
				"params": map[string]any{"type": "array", "items": map[string]any{}, "description": "Positional query parameters."},
				"limit":  prop("integer", "Maximum rows to return."),
			}),
		tool("explain", "Return the query plan for a statement without executing it.",
			[]string{"sql"}, map[string]any{
				"sql":     prop("string", "Statement to explain."),
				"analyze": prop("boolean", "Run EXPLAIN ANALYZE instead of a plain plan."),
			}),
		tool("list_tables", "List tables in a schema with row estimates and sizes.",
			nil, map[string]any{"schema": prop("string", "Schema name, defaults to public.")}),
		tool("describe_table", "Describe a table's columns, types, defaults, indexes and constraints.",
			[]string{"table"}, map[string]any{
				"table":  prop("string", "Table name."),
				"schema": prop("string", "Schema name."),
			}),
		tool("list_indexes", "List indexes on a table with definitions and sizes.",
			[]string{"table"}, map[string]any{
				"table":  prop("string", "Table name."),
				"schema": prop("string", "Schema name."),
			}),
		tool("list_migrations", "List applied migrations with versions and timestamps.",
			nil, map[string]any{"limit": prop("integer", "Maximum migrations to return.")}),
	}

	servers["sentry"] = []invToolDef{
		tool("list_issues", "List unresolved issues for a project, ordered by event count or last seen.",
			[]string{"project"}, map[string]any{
				"project":     prop("string", "Project slug."),
				"query":       prop("string", "Sentry search query."),
				"statsPeriod": prop("string", "Stats period such as 24h or 14d."),
			}),
		tool("get_issue", "Fetch one issue with its culprit, metadata and event counts.",
			[]string{"issueId"}, map[string]any{"issueId": prop("string", "Numeric issue id.")}),
		tool("get_latest_event", "Fetch the most recent event for an issue including the full stack trace.",
			[]string{"issueId"}, map[string]any{"issueId": prop("string", "Numeric issue id.")}),
		tool("resolve_issue", "Mark an issue resolved, optionally in the next release.",
			[]string{"issueId"}, map[string]any{
				"issueId":   prop("string", "Numeric issue id."),
				"inRelease": prop("string", "Release version that resolves the issue."),
			}),
		tool("list_releases", "List releases with commit counts and deploy status.",
			[]string{"project"}, map[string]any{
				"project": prop("string", "Project slug."),
				"perPage": prop("integer", "Results per page."),
			}),
	}

	servers["slack"] = []invToolDef{
		tool("post_message", "Post a message to a channel, optionally in a thread.",
			[]string{"channel", "text"}, map[string]any{
				"channel":  prop("string", "Channel id or name."),
				"text":     prop("string", "Message text in mrkdwn."),
				"threadTs": prop("string", "Timestamp of the parent message to reply to."),
			}),
		tool("list_channels", "List channels the bot can see, with member counts and topics.",
			nil, map[string]any{
				"types": prop("string", "Comma separated channel types."),
				"limit": prop("integer", "Page size."),
			}),
		tool("search_messages", "Search messages across channels with Slack search syntax.",
			[]string{"query"}, map[string]any{
				"query": prop("string", "Search query."),
				"count": prop("integer", "Results per page."),
			}),
		tool("get_thread", "Fetch every reply in a thread.",
			[]string{"channel", "threadTs"}, map[string]any{
				"channel":  prop("string", "Channel id."),
				"threadTs": prop("string", "Parent message timestamp."),
			}),
		tool("upload_file", "Upload a file or snippet to a channel.",
			[]string{"channel", "content"}, map[string]any{
				"channel":  prop("string", "Channel id."),
				"content":  prop("string", "File contents."),
				"filename": prop("string", "File name."),
				"filetype": prop("string", "Syntax type for snippets."),
			}),
	}

	servers["fetch"] = []invToolDef{
		tool("fetch_url", "Fetch a URL and return the response body converted to Markdown.",
			[]string{"url"}, map[string]any{
				"url":      prop("string", "Absolute URL to fetch."),
				"maxBytes": prop("integer", "Truncate the response after this many bytes."),
				"headers":  map[string]any{"type": "object", "description": "Additional request headers."},
			}),
		tool("fetch_json", "Fetch a URL and parse the response as JSON.",
			[]string{"url"}, map[string]any{
				"url":    prop("string", "Absolute URL to fetch."),
				"method": prop("string", "HTTP method."),
				"body":   prop("string", "Request body for POST or PUT."),
			}),
		tool("extract_links", "Fetch a page and return every outbound link with its anchor text.",
			[]string{"url"}, map[string]any{
				"url":      prop("string", "Absolute URL to fetch."),
				"sameHost": prop("boolean", "Only return links on the same host."),
			}),
	}

	type inventoryOut struct {
		Meta    map[string]string         `json:"meta"`
		Servers map[string]map[string]any `json:"servers"`
	}
	out := inventoryOut{
		Meta: map[string]string{
			"generated_by": "internal/demo/gen.go",
			"purpose":      "synthetic MCP tool inventory for `aiprobe demo`",
			"note":         "not a real deployment; tool names and schemas are representative of common MCP servers",
		},
		Servers: map[string]map[string]any{},
	}
	total, schemaTokens := 0, 0
	for name, tools := range servers {
		out.Servers[name] = map[string]any{"tools": tools}
		total += len(tools)
		for _, t := range tools {
			schemaTokens += schemaCost(t)
		}
	}

	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		panic(err)
	}
	b = append(b, '\n')
	path := filepath.Join("internal", "demo", "testdata", "demo-mcp-tools.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		panic(err)
	}
	fmt.Printf("wrote %s (%d servers, %d tools, ~%d schema tokens)\n", path, len(servers), total, schemaTokens)
	return schemaTokens
}

// schemaCost mirrors the estimate in internal/analyzer/mcp: tool name plus
// description plus compact input schema plus a small per-tool overhead.
func schemaCost(t invToolDef) int {
	const perToolOverhead = 8
	b, err := json.Marshal(t.InputSchema)
	if err != nil {
		panic(err)
	}
	return tokenize.Estimate(t.Name) + tokenize.Estimate(t.Description) + perToolOverhead + tokenize.Estimate(string(b))
}
