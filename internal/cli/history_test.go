package cli

import (
	"bytes"
	"github.com/NextProbe/aiprobe/internal/pathutil"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/report"
	"github.com/NextProbe/aiprobe/internal/store"
	"github.com/NextProbe/aiprobe/internal/trace"
)

// historyHome points the store at a temporary home and fills it with analyses.
//
// Titles are deliberately Chinese: a session summary is user text, so a table that
// only ever sees ASCII in tests will misalign the moment someone works in Chinese.
func historyHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv(pathutil.HomeEnvVar(), home)

	st, err := store.New(store.Options{Version: version})
	if err != nil {
		t.Fatal(err)
	}
	put := func(id, title string, size int64, sc int, grade string) {
		res := &analyzer.Result{
			Meta: analyzer.Meta{
				Adapter: "kiro", SessionID: id, Requests: 40, ToolCalls: 12,
				PrimaryModel: "claude-opus-5", Summary: title,
			},
			Headline: analyzer.Headline{
				TotalContextTokens: 196382267, PotentialSaving: 12392746,
				MCPContextWaste: trace.UnknownInt(), CacheMissWaste: trace.UnknownInt(),
			},
			Score: &score.Score{Value: sc, Grade: grade, KnownWeight: 100},
		}
		if _, err := st.Put(res, store.PutMeta{
			Path: "/sessions/" + id + ".jsonl", Title: title, SizeBytes: size,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Two analyses of one session, so a trend line is printed too, plus a second
	// session so the table has more than one row.
	put("sess-a", "帮我做个简单的登陆吧 现在这样太不安全了", 100, 70, "B")
	put("sess-a", "帮我做个简单的登陆吧 现在这样太不安全了", 200, 84, "B")
	put("sess-b", "Implement requirements from req.md", 100, 95, "A")
	return home
}

func historyOutput(t *testing.T, args ...string) string {
	t.Helper()
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, errOut.String())
	}
	return out.String()
}

// The terminal history table is padded in display cells, not runes. A CJK glyph is
// one rune and two cells, so padding by rune count makes the columns drift apart
// exactly where the text is widest — which is the whole table, in Chinese.
//
// The invariant is not that the two languages produce the same headings. It is
// that the columns land in the same cells, which is what the data rows prove:
// they carry no translated text, so if they differ, the padding shifted.
func TestHistoryTableAlignsInBothLanguages(t *testing.T) {
	historyHome(t)

	dataRows := func(out string) []string {
		var rows []string
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, " 20") { // a timestamped row
				rows = append(rows, line)
			}
		}
		return rows
	}

	en := dataRows(historyOutput(t, "history", "--lang", "en"))
	zh := dataRows(historyOutput(t, "history", "--lang", "zh"))

	if len(en) != 3 {
		t.Fatalf("expected 3 stored analyses, got %d rows:\n%s", len(en), strings.Join(en, "\n"))
	}
	if len(en) != len(zh) {
		t.Fatalf("row counts differ: en=%d zh=%d", len(en), len(zh))
	}
	for i := range en {
		if en[i] != zh[i] {
			t.Errorf("row %d differs between languages, so a column shifted:\n  en: %q\n  zh: %q",
				i, en[i], zh[i])
		}
	}

	// And every heading has to fit its column, or it pushes the rest along.
	for _, lang := range []string{"en", "zh"} {
		out := historyOutput(t, "history", "--lang", lang)
		header := ""
		for _, line := range strings.Split(out, "\n") {
			if strings.TrimSpace(line) != "" {
				header = line
				break
			}
		}
		if got := report.WidthCells(header); got > 102 {
			t.Errorf("%s heading row is %d cells wide, too wide for a 100-column table: %q",
				lang, got, header)
		}
		for _, row := range dataRows(out) {
			if got := report.WidthCells(row); got > 102 {
				t.Errorf("%s row is %d cells wide: %q", lang, got, row)
			}
		}
	}
}

// The history command is one of the two places a user reads their own data, so it
// has to speak both languages like everything else that renders a report.
func TestHistoryIsLocalized(t *testing.T) {
	historyHome(t)

	en := historyOutput(t, "history", "--lang", "en")
	zh := historyOutput(t, "history", "--lang", "zh")

	for _, want := range []string{"Analysed", "Runtime", "Recoverable", "Score", "Stored in"} {
		if !strings.Contains(en, want) {
			t.Errorf("English history is missing %q", want)
		}
	}
	for _, want := range []string{"分析时间", "运行时", "可回收", "评分", "保存在"} {
		if !strings.Contains(zh, want) {
			t.Errorf("Chinese history is missing %q", want)
		}
	}
	// The trend line, which is the reason to keep a history at all.
	if !strings.Contains(en, "70 → 84") || !strings.Contains(en, "improving") {
		t.Errorf("English history should report the trend:\n%s", en)
	}
	if !strings.Contains(zh, "70 → 84") || !strings.Contains(zh, "在变好") {
		t.Errorf("Chinese history should report the trend:\n%s", zh)
	}
}

func TestHistoryRejectsAnUnsupportedLanguage(t *testing.T) {
	historyHome(t)
	cmd := newRootCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs([]string{"history", "--lang", "kl"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("an unsupported --lang should be refused, as it is on every other command")
	}
}

// --path and --clear are how the privacy documentation says to inspect and delete
// the history, so they have to do what it claims.
func TestHistoryPathAndClear(t *testing.T) {
	home := historyHome(t)

	path := strings.TrimSpace(historyOutput(t, "history", "--path"))
	if !strings.HasPrefix(path, home) {
		t.Errorf("--path printed %q, which is not inside the redirected home %q", path, home)
	}

	if out := historyOutput(t, "history", "--clear"); !strings.Contains(out, "3") {
		t.Errorf("--clear should report how many analyses it removed, got %q", out)
	}
	if out := historyOutput(t, "history"); !strings.Contains(out, "No analyses stored yet") {
		t.Errorf("history should be empty after --clear, got:\n%s", out)
	}
}
