package html

import (
	"regexp"
	"strings"
	"testing"

	"github.com/NextProbe/aiprobe/internal/analyzer"
	analyzerctx "github.com/NextProbe/aiprobe/internal/analyzer/context"
	"github.com/NextProbe/aiprobe/internal/analyzer/overview"
	"github.com/NextProbe/aiprobe/internal/analyzer/rules"
	"github.com/NextProbe/aiprobe/internal/analyzer/score"
	"github.com/NextProbe/aiprobe/internal/i18n"
	"github.com/NextProbe/aiprobe/internal/trace"
)

func sampleOverview() *overview.Report {
	mk := func(id string, ctx, saving, sc int, grade string) overview.Input {
		return overview.Input{
			ID:    "kiro:" + id,
			Title: "session " + id,
			Result: &analyzer.Result{
				Meta: analyzer.Meta{
					Adapter: "kiro", SessionID: id, SourcePath: "/s/" + id,
					Requests: 40, ToolCalls: 12, DurationSecs: 3600,
					PrimaryModel: "claude-opus-5", CostUnit: "credit", CostAmount: 12.5,
				},
				Headline: analyzer.Headline{
					TotalContextTokens: ctx, DuplicateContext: saving,
					MCPContextWaste: trace.UnknownInt(), CacheMissWaste: trace.UnknownInt(),
					PotentialSaving: saving, PotentialSavingRatio: float64(saving) / float64(ctx),
				},
				Score: &score.Score{Value: sc, Grade: grade, KnownWeight: 100},
				Breakdown: &analyzerctx.Breakdown{Categories: []analyzerctx.CategoryLine{
					{Category: trace.CatMessages, Billed: trace.Amount{Tokens: ctx / 2, Method: trace.MethodEstimator}},
					{Category: trace.CatFileRead, Billed: trace.Amount{Tokens: ctx / 2, Method: trace.MethodEstimator}},
				}},
				Recommendations: []rules.Recommendation{{
					ID: "R003", Area: rules.AreaDuplicate,
					Issue: "the same files are read repeatedly", Fix: "read ranges instead",
					IssueMsg:     i18n.Msg{ID: "rule.R003.issue"},
					FixMsg:       i18n.Msg{ID: "rule.R003.fix"},
					WastedTokens: trace.KnownInt(saving), Confidence: trace.ConfHigh,
				}},
				Tools: []analyzer.ToolRow{
					{Name: "fsWrite", Class: trace.ToolClassFileWrite, Calls: 9, ResultTokens: 100, BilledTokens: saving},
				},
			},
		}
	}
	return overview.Build(
		[]overview.Input{mk("aaa", 200000, 40000, 30, "F"), mk("bbb", 100000, 5000, 80, "B")},
		[]overview.Failure{{Path: "/s/broken", Error: "unexpected EOF"}},
		overview.Options{Adapter: "kiro"})
}

func TestRenderOverview(t *testing.T) {
	var b strings.Builder
	err := RenderOverview(&b, sampleOverview(), OverviewOptions{
		Locale: i18n.EN, Version: "test",
		LanguageHref: "/overview?adapter=kiro&lang={locale}",
		ReportHref:   "/report?id={id}",
	})
	if err != nil {
		t.Fatalf("RenderOverview: %v", err)
	}
	out := b.String()

	for _, want := range []string{
		"<html", "</html>", "AIProbe",
		"Across all",          // heading text from the catalog
		"panel-habits",        // the aggregated findings tab
		"panel-sessions",      // the ranked session list
		"unexpected EOF",      // failures are surfaced, not swallowed
		"/report?id=kiro:aaa", // each row links to its own report
		"session aaa",         // session titles are shown
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered overview should contain %q", want)
		}
	}
	// No unrendered message ids should reach the page.
	if m := regexp.MustCompile(`>overview\.[a-z_.]+<`).FindString(out); m != "" {
		t.Errorf("an untranslated id reached the output: %s", m)
	}
}

func TestRenderOverviewChinese(t *testing.T) {
	var b strings.Builder
	if err := RenderOverview(&b, sampleOverview(), OverviewOptions{Locale: i18n.ZH, Version: "test"}); err != nil {
		t.Fatalf("RenderOverview: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, `lang="zh`) {
		t.Error("the Chinese overview should declare a zh lang attribute")
	}
	for _, want := range []string{"全部会话汇总", "习惯性问题", "可回收"} {
		if !strings.Contains(out, want) {
			t.Errorf("Chinese overview should contain %q", want)
		}
	}
}

// A standalone file has no server to link to, so the session rows must render
// without an open button rather than emitting a dead link.
func TestRenderOverviewWithoutLinks(t *testing.T) {
	var b strings.Builder
	if err := RenderOverview(&b, sampleOverview(), OverviewOptions{Locale: i18n.EN, Version: "test"}); err != nil {
		t.Fatalf("RenderOverview: %v", err)
	}
	out := b.String()
	if strings.Contains(out, "/report?id=") {
		t.Error("a standalone overview must not link to server routes")
	}
	// A standalone file has nowhere to navigate to, and a dead nav bar is worse
	// than none.
	if strings.Contains(out, `class="sitenav`) {
		t.Error("a standalone overview must not render site navigation")
	}
	if strings.Contains(out, `class="navlink`) {
		t.Error("a standalone overview must not render nav links")
	}
}

// The report promises it makes no outbound requests.
func TestOverviewReferencesNoExternalOrigin(t *testing.T) {
	var b strings.Builder
	if err := RenderOverview(&b, sampleOverview(), OverviewOptions{Locale: i18n.EN}); err != nil {
		t.Fatal(err)
	}
	allowed := regexp.MustCompile(`^https?://(www\.)?w3\.org/`)
	for _, u := range regexp.MustCompile(`https?://[^\s"'<>)]+`).FindAllString(b.String(), -1) {
		if !allowed.MatchString(u) {
			t.Errorf("overview references an external origin: %s", u)
		}
	}
}

// An empty set still has to produce a readable page, since that is what someone
// with no sessions for a runtime will see.
func TestRenderEmptyOverview(t *testing.T) {
	var b strings.Builder
	empty := overview.Build(nil, nil, overview.Options{Adapter: "claude-code"})
	if err := RenderOverview(&b, empty, OverviewOptions{Locale: i18n.EN, Version: "test"}); err != nil {
		t.Fatalf("RenderOverview on an empty set: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "<html") {
		t.Error("an empty overview should still be a document")
	}
	if !strings.Contains(out, "no sessions found") {
		t.Error("an empty overview should say there are no sessions")
	}
}

// When the server does supply navigation, it has to render, mark the current
// page, and sit alongside the language switcher rather than replacing it.
func TestRenderOverviewWithNav(t *testing.T) {
	var b strings.Builder
	err := RenderOverview(&b, sampleOverview(), OverviewOptions{
		Locale:       i18n.EN,
		Version:      "test",
		LanguageHref: "/overview?adapter=kiro&lang={locale}",
		Nav: []NavLink{
			{Label: "Sessions", Href: "/"},
			{Label: "History", Href: "/history"},
			{Label: "Overview", Href: "/overview?adapter=kiro", Current: true},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := b.String()

	if !strings.Contains(out, `class="sitenav`) {
		t.Fatal("site navigation should render when supplied")
	}
	for _, want := range []string{
		`href="/"`,
		`href="/history"`,
		`class="navlink on"`,  // the current page is marked
		`aria-current="page"`, // and marked for assistive technology too
		"Sessions", "History",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("nav should contain %q", want)
		}
	}
	// The language switcher must survive alongside it.
	if !strings.Contains(out, "sitenav-lang") {
		t.Error("the language switcher should still render next to the nav")
	}
}
