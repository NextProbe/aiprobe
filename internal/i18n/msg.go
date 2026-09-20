package i18n

// Deferred messages
//
// Analyzers run before anything is rendered, and they do not know what language
// the report will be printed in. A serve process renders the same analysis in
// two languages from one run. So an analyzer that wants to say something records
// what it wants to say, not the sentence itself: a message id plus the values
// its placeholders need. The reporter renders it later, in whatever locale it
// was asked for.
//
// This also keeps English out of the analyzer packages. The English wording
// lives only in the catalog, which means the JSON text and the translated text
// are rendered from the same template and cannot drift apart.

// Msg is a message that has not been rendered yet.
type Msg struct {
	ID   string `json:"id"`
	Args Args   `json:"args,omitempty"`
}

// Ref is an argument whose value is itself a message id.
//
// It exists because some sentences embed a term that also needs translating: a
// cache drift cause, a token category. Concatenating the rendered term in the
// analyzer would fix the word order to English, so the term is passed as a
// reference and resolved at render time, in the target locale.
type Ref string

// Msgf builds a message.
func Msgf(id string, args Args) Msg { return Msg{ID: id, Args: args} }

// Empty reports whether the message carries nothing to render.
func (m Msg) Empty() bool { return m.ID == "" }

// Render renders the message in loc, resolving Ref arguments in the same locale.
func (m Msg) Render(loc Locale) string {
	if m.ID == "" {
		return ""
	}
	return T(loc, m.ID, resolve(loc, m.Args))
}

// EN renders the message in English. Reports keep an English copy of every
// sentence so a JSON report is identical whatever locale produced it, and so
// results stay comparable across machines.
func (m Msg) EN() string { return m.Render(EN) }

// resolve replaces Ref and Msg arguments with their rendering in loc, so a
// sentence can be built out of translated pieces without the caller deciding
// where in the sentence they belong.
func resolve(loc Locale, args Args) Args {
	if len(args) == 0 {
		return args
	}
	var out Args
	for k, v := range args {
		var rendered string
		switch x := v.(type) {
		case Ref:
			rendered = T(loc, string(x), nil)
		case Msg:
			rendered = x.Render(loc)
		default:
			continue
		}
		if out == nil {
			// Copied lazily: most messages have no references, and mutating the
			// analyzer's own map would make the rendering locale-sticky.
			out = make(Args, len(args))
			for k2, v2 := range args {
				out[k2] = v2
			}
		}
		out[k] = rendered
	}
	if out == nil {
		return args
	}
	return out
}

// Msg renders a deferred message in the printer's locale.
func (p *Printer) Msg(m Msg) string {
	if p == nil {
		return m.Render(Default)
	}
	return m.Render(p.Locale)
}

// Msgs renders a list of deferred messages.
func (p *Printer) Msgs(ms []Msg) []string {
	if len(ms) == 0 {
		return nil
	}
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, p.Msg(m))
	}
	return out
}

// Lines is a list of deferred messages that also keeps the English rendering,
// which is what analyzers accumulate while they work.
type Lines struct {
	EN   []string
	Msgs []Msg
}

// Add appends a message, rendering the English form immediately.
func (l *Lines) Add(id string, args Args) {
	m := Msg{ID: id, Args: args}
	l.EN = append(l.EN, m.EN())
	l.Msgs = append(l.Msgs, m)
}

// Len reports how many lines have been added.
func (l *Lines) Len() int { return len(l.Msgs) }

// Truncate keeps the first n lines and appends a note about the remainder, so
// both representations stay the same length and describe the same list.
func (l *Lines) Truncate(n int, moreID string) {
	if n < 0 || l.Len() <= n {
		return
	}
	extra := l.Len() - n
	l.EN = l.EN[:n]
	l.Msgs = l.Msgs[:n]
	l.Add(moreID, Args{"count": extra})
}
