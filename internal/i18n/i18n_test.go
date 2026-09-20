package i18n

import (
	"sort"
	"strings"
	"testing"
)

// TestEveryLocaleCoversEnglish is the guard that keeps translations from rotting.
// English is the source language, so any id it defines must exist in every other
// locale. Without this, adding a string silently degrades other languages into a
// mix of two.
func TestEveryLocaleCoversEnglish(t *testing.T) {
	english := catalogs[EN]
	if len(english) == 0 {
		t.Fatal("the English catalog is empty")
	}

	for _, loc := range Locales() {
		if loc == EN {
			continue
		}
		var missing []string
		for id := range english {
			if v, ok := catalogs[loc][id]; !ok || strings.TrimSpace(v) == "" {
				missing = append(missing, id)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("locale %q is missing %d translation(s):\n  %s",
				loc, len(missing), strings.Join(missing, "\n  "))
		}
	}
}

// TestNoLocaleInventsIDs catches the opposite mistake: a translation for an id
// that no longer exists is dead weight and usually means a rename was only half
// applied.
func TestNoLocaleInventsIDs(t *testing.T) {
	english := catalogs[EN]
	for _, loc := range Locales() {
		if loc == EN {
			continue
		}
		var extra []string
		for id := range catalogs[loc] {
			if _, ok := english[id]; !ok {
				extra = append(extra, id)
			}
		}
		sort.Strings(extra)
		if len(extra) > 0 {
			t.Errorf("locale %q defines %d id(s) that English does not:\n  %s",
				loc, len(extra), strings.Join(extra, "\n  "))
		}
	}
}

// TestPlaceholdersMatch is the correctness guard for named arguments. If a
// translation drops or renames a placeholder, the rendered string loses a value
// or leaks a literal brace, and both are silent failures at runtime.
func TestPlaceholdersMatch(t *testing.T) {
	english := catalogs[EN]
	for _, loc := range Locales() {
		if loc == EN {
			continue
		}
		for id, src := range english {
			dst, ok := catalogs[loc][id]
			if !ok {
				continue // reported by the coverage test
			}
			want := placeholders(src)
			got := placeholders(dst)
			if !equalSets(want, got) {
				t.Errorf("%s[%s]: placeholders %v, English has %v", loc, id, got, want)
			}
		}
	}
}

func placeholders(s string) []string {
	var out []string
	for {
		open := strings.IndexByte(s, '{')
		if open < 0 {
			break
		}
		close := strings.IndexByte(s[open:], '}')
		if close < 0 {
			break
		}
		close += open
		out = append(out, s[open+1:close])
		s = s[close+1:]
	}
	sort.Strings(out)
	return out
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestExpand(t *testing.T) {
	cases := []struct {
		name string
		text string
		args Args
		want string
	}{
		{"no args", "plain text", nil, "plain text"},
		{"one arg", "{n} tokens", Args{"n": 42}, "42 tokens"},
		{"repeated", "{n} of {n}", Args{"n": "x"}, "x of x"},
		{"reordered", "{b} then {a}", Args{"a": "A", "b": "B"}, "B then A"},
		{"missing arg leaves marker", "{a} {b}", Args{"a": "A"}, "A {b}"},
		{"unclosed brace", "{a unclosed", Args{"a": "A"}, "{a unclosed"},
		{"no placeholders but args", "plain", Args{"a": "A"}, "plain"},
		{"nil value", "[{a}]", Args{"a": nil}, "[]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := expand(tc.text, tc.args); got != tc.want {
				t.Errorf("expand(%q) = %q, want %q", tc.text, got, tc.want)
			}
		})
	}
}

// TestReorderingIsPossible is the reason placeholders are named. A translation
// must be able to put values in a different order from the source.
func TestReorderingIsPossible(t *testing.T) {
	register("test-rtl", map[string]string{"x": "{second} before {first}"})
	defer delete(catalogs, "test-rtl")
	register(EN, map[string]string{"x": "{first} before {second}"})
	defer delete(catalogs[EN], "x")

	got := T("test-rtl", "x", Args{"first": "A", "second": "B"})
	if got != "B before A" {
		t.Errorf("got %q, want the translation's own order", got)
	}
}

func TestFallbackToEnglish(t *testing.T) {
	register(EN, map[string]string{"only.english": "english text"})
	defer delete(catalogs[EN], "only.english")

	if got := T(ZH, "only.english", nil); got != "english text" {
		t.Errorf("T(zh) = %q, want the English fallback", got)
	}
}

func TestMissingIDReturnsTheID(t *testing.T) {
	// Surfacing the identifier makes an untranslated string obvious in review,
	// where an empty string would look like an intentional blank.
	if got := T(EN, "no.such.id", nil); got != "no.such.id" {
		t.Errorf("got %q, want the id echoed back", got)
	}
}

func TestParse(t *testing.T) {
	cases := map[string]Locale{
		"":            Default,
		"en":          EN,
		"EN":          EN,
		"en-US":       EN,
		"en_US.UTF-8": EN,
		"zh":          ZH,
		"zh-CN":       ZH,
		"zh_CN.UTF-8": ZH,
		"ZH-Hans":     ZH,
		"cn":          ZH,
		"fr":          Default,
		"klingon":     Default,
	}
	for in, want := range cases {
		if got := Parse(in); got != want {
			t.Errorf("Parse(%q) = %q, want %q", in, got, want)
		}
	}
}

// ParseStrict exists to tell "this names English" apart from "this is unknown, so
// English". Parse cannot express the difference, which is why `--lang zn` used to
// produce an English report instead of an error.
func TestParseStrictSeparatesEnglishFromFallback(t *testing.T) {
	recognised := map[string]Locale{
		"en":          EN,
		"EN":          EN,
		"en-US":       EN,
		"en_US.UTF-8": EN,
		"zh":          ZH,
		"zh-CN":       ZH,
		"zh_CN.UTF-8": ZH,
		"ZH-Hans":     ZH,
		"cn":          ZH,
		"chs":         ZH,
		"hans":        ZH,
	}
	for in, want := range recognised {
		got, ok := ParseStrict(in)
		if !ok {
			t.Errorf("ParseStrict(%q) reported the language as unknown", in)
			continue
		}
		if got != want {
			t.Errorf("ParseStrict(%q) = %q, want %q", in, got, want)
		}
	}

	// Anything not naming a supported language has to be reported as such, or a
	// typo silently becomes a different language.
	for _, in := range []string{"", " ", "zn", "kl", "fr", "de-DE", "klingon", "e", "zz"} {
		if got, ok := ParseStrict(in); ok {
			t.Errorf("ParseStrict(%q) = %q, accepted; it names no supported language", in, got)
		}
	}
}

func TestLocalesAndNames(t *testing.T) {
	locs := Locales()
	if len(locs) < 2 {
		t.Fatalf("Locales = %v, want at least English and one translation", locs)
	}
	if locs[0] != EN {
		t.Errorf("Locales[0] = %q, want English first", locs[0])
	}
	for _, loc := range locs {
		if Name(loc) == "" {
			t.Errorf("locale %q has no display name", loc)
		}
		if !Supported(loc) {
			t.Errorf("locale %q is listed but not supported", loc)
		}
	}
	if Name("nonexistent") != "nonexistent" {
		t.Error("Name should echo an unknown locale")
	}
}

func TestPrinter(t *testing.T) {
	p := New(ZH)
	if p.Locale != ZH {
		t.Errorf("Locale = %q", p.Locale)
	}
	if !p.IsZH() {
		t.Error("IsZH = false for the Chinese printer")
	}
	if got := p.S("headline.score"); got == "headline.score" {
		t.Error("the Chinese headline is untranslated")
	}
	if got := New(EN).S("headline.score"); got != "Context Efficiency" {
		t.Errorf("English headline = %q", got)
	}
	// An unsupported locale degrades to the default rather than failing.
	if New("klingon").Locale != Default {
		t.Error("an unsupported locale should fall back to the default")
	}
	// A nil printer must not panic, so reporters can hold an optional one.
	var nilp *Printer
	if got := nilp.T("headline.score", nil); got != "Context Efficiency" {
		t.Errorf("nil printer = %q, want the English default", got)
	}
}

// TestChineseIsActuallyTranslated spot-checks that the Chinese catalog contains
// Chinese, catching a copy-paste of the English file.
func TestChineseIsActuallyTranslated(t *testing.T) {
	checked := 0
	for id, text := range catalogs[ZH] {
		// A short list of ids is intentionally identical across languages: the
		// product name, and acronyms developers use untranslated.
		switch {
		case strings.HasPrefix(id, "area."), id == "tab.json",
			id == "app.name", id == "serve.title",
			// "prompt" and "schema" are the terms Chinese developers use, and
			// the terminal columns they head are too narrow for a gloss.
			id == "col.short.prompt", id == "col.short.schema",
			id == "toolclass.mcp",
			// Compact button labels that are a glyph plus a format name.
			strings.HasSuffix(id, ".short") && strings.HasPrefix(id, "serve.download"),
			// Assembled purely from other messages, so it has no words of its
			// own; the clauses it joins are checked individually.
			id == "consistency.verdict":
			continue
		}
		if !hasCJK(text) {
			t.Errorf("zh[%s] = %q contains no Chinese characters", id, text)
		}
		checked++
	}
	if checked < 50 {
		t.Errorf("only %d Chinese strings checked, expected a full catalog", checked)
	}
}

func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x3000 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
