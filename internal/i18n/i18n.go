// Package i18n renders AIProbe's user-facing text in a chosen language.
//
// # Design notes
//
// Messages are looked up by a stable identifier and formatted with *named*
// placeholders, not positional ones. That matters more than it might seem:
// Chinese and English order clauses differently, so a translation frequently
// needs to move a value to a different part of the sentence. Positional
// formatting would make that impossible without silently mismatching arguments.
//
//	i18n.T(i18n.ZH, "rule.R003.issue", nil)
//	i18n.T(i18n.EN, "breakdown.overhead", i18n.Args{"tokens": "4,589", "share": "8%"})
//
// A missing translation falls back to English rather than showing a raw
// identifier, and a missing English entry returns the identifier so the gap is
// obvious in review instead of silently rendering an empty string.
package i18n

import (
	"sort"
	"strings"
)

// Locale identifies a supported language.
type Locale string

const (
	// EN is the source language and the fallback for every other locale.
	EN Locale = "en"
	// ZH is Simplified Chinese.
	ZH Locale = "zh"
)

// Default is used when no locale is requested.
const Default = EN

// Args holds named placeholder values.
type Args map[string]any

// catalogs holds every translation, keyed by locale then message id.
var catalogs = map[Locale]map[string]string{}

// register adds a catalog. Called from the per-language files at init.
func register(loc Locale, messages map[string]string) {
	if catalogs[loc] == nil {
		catalogs[loc] = map[string]string{}
	}
	for id, text := range messages {
		catalogs[loc][id] = text
	}
}

// Locales lists the supported locales, English first.
func Locales() []Locale {
	out := make([]Locale, 0, len(catalogs))
	for loc := range catalogs {
		if loc == EN {
			continue
		}
		out = append(out, loc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return append([]Locale{EN}, out...)
}

// Names maps locales to their own endonym, for a language picker.
var Names = map[Locale]string{
	EN: "English",
	ZH: "简体中文",
}

// Name returns the display name of a locale.
func Name(loc Locale) string {
	if n, ok := Names[loc]; ok {
		return n
	}
	return string(loc)
}

// Parse resolves a locale string, tolerating case and region subtags such as
// "zh-CN" or "en_US". Unknown input falls back to the default rather than
// erroring, because a language preference is never worth failing a run over.
func Parse(s string) Locale {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return Default
	}
	s = strings.ReplaceAll(s, "_", "-")
	// Strip any encoding suffix from POSIX-style values like zh_CN.UTF-8.
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	base := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		base = s[:i]
	}
	for _, loc := range Locales() {
		if base == string(loc) {
			return loc
		}
	}
	switch base {
	case "cn", "zh", "chs", "hans":
		return ZH
	}
	return Default
}

// ParseStrict resolves a language tag and reports whether it was recognised.
//
// It exists because Parse deliberately never fails: an environment variable that
// names an unsupported language should not stop a run. An explicit `--lang` is a
// different case. Someone who types `--lang zn` asked for a language, and quietly
// giving them a different one is worse than telling them the flag was wrong. That
// distinction is only expressible if the caller can see the fallback happen.
func ParseStrict(s string) (Locale, bool) {
	if strings.TrimSpace(s) == "" {
		return Default, false
	}
	loc := Parse(s)
	// Parse falls back to Default, so a resolved Default has to be confirmed
	// against the input rather than trusted.
	if loc == Default && !namesLocale(s, Default) {
		return Default, false
	}
	return loc, true
}

// namesLocale reports whether s actually names loc, as opposed to having fallen
// back to it.
func namesLocale(s string, loc Locale) bool {
	base := strings.ToLower(strings.TrimSpace(s))
	base = strings.ReplaceAll(base, "_", "-")
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	if i := strings.IndexByte(base, '-'); i >= 0 {
		base = base[:i]
	}
	if base == string(loc) {
		return true
	}
	if loc == ZH {
		switch base {
		case "cn", "zh", "chs", "hans":
			return true
		}
	}
	return false
}

// Supported reports whether a locale has a catalog.
func Supported(loc Locale) bool {
	_, ok := catalogs[loc]
	return ok
}

// T renders a message. Placeholders of the form {name} are replaced with the
// corresponding value from args.
func T(loc Locale, id string, args Args) string {
	text, ok := lookup(loc, id)
	if !ok {
		// Returning the id makes an untranslated string visible rather than
		// producing a confusingly blank report.
		return id
	}
	return expand(text, args)
}

// Has reports whether an id exists in the given locale or in English.
func Has(loc Locale, id string) bool {
	_, ok := lookup(loc, id)
	return ok
}

func lookup(loc Locale, id string) (string, bool) {
	if c, ok := catalogs[loc]; ok {
		if text, ok := c[id]; ok && text != "" {
			return text, true
		}
	}
	if loc != EN {
		if text, ok := catalogs[EN][id]; ok && text != "" {
			return text, true
		}
	}
	return "", false
}

// expand substitutes {name} placeholders. Unknown placeholders are left in
// place, which surfaces a translation bug instead of hiding it.
func expand(text string, args Args) string {
	if len(args) == 0 || !strings.ContainsRune(text, '{') {
		return text
	}
	var b strings.Builder
	b.Grow(len(text) + 16)
	for {
		open := strings.IndexByte(text, '{')
		if open < 0 {
			b.WriteString(text)
			break
		}
		close := strings.IndexByte(text[open:], '}')
		if close < 0 {
			b.WriteString(text)
			break
		}
		close += open
		b.WriteString(text[:open])
		name := text[open+1 : close]
		if v, ok := args[name]; ok {
			b.WriteString(str(v))
		} else {
			b.WriteString(text[open : close+1])
		}
		text = text[close+1:]
	}
	return b.String()
}

func str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return sprint(x)
	}
}

// Printer binds a locale so callers do not repeat it on every lookup.
type Printer struct {
	Locale Locale
}

// New returns a Printer for the locale.
func New(loc Locale) *Printer {
	if !Supported(loc) {
		loc = Default
	}
	return &Printer{Locale: loc}
}

// T renders a message in the printer's locale.
func (p *Printer) T(id string, args Args) string {
	if p == nil {
		return T(Default, id, args)
	}
	return T(p.Locale, id, args)
}

// S renders a message with no arguments.
func (p *Printer) S(id string) string { return p.T(id, nil) }

// IsZH reports whether the printer renders Chinese, which a few layout
// decisions depend on because CJK glyphs are double width in a terminal.
func (p *Printer) IsZH() bool { return p != nil && p.Locale == ZH }
