package report

import (
	"encoding/json"
	"io"

	"github.com/NextProbe/aiprobe/internal/analyzer"
)

// SchemaVersion identifies the JSON report contract. Consumers should check it
// before relying on field names.
const SchemaVersion = "aiprobe.report/v0.1"

// jsonEnvelope wraps the analysis so the schema version travels with the data.
type jsonEnvelope struct {
	Schema string `json:"schema"`
	*analyzer.Result
}

// JSONOptions controls optional content of the JSON report.
type JSONOptions struct {
	Indent bool
	// Previews are written into questions.items[].preview only when non-nil,
	// and questions.previews_included is then true.
	Previews map[int]string
}

// WriteJSON emits the machine-readable report.
//
// The analysis struct deliberately excludes the parsed trace, so no file content
// or command output can leak into the JSON, and no prompt text either except the
// one-line, credential-scrubbed question previews a caller passed through
// WithPreviews (--prompt-preview). Only counts, hashes, paths, session titles and
// short redacted excerpts are serialized otherwise.
func WriteJSON(w io.Writer, res *analyzer.Result, indent bool) error {
	return WriteJSONOptions(w, res, JSONOptions{Indent: indent})
}

// WriteJSONOptions emits the machine-readable report with optional content.
func WriteJSONOptions(w io.Writer, res *analyzer.Result, opts JSONOptions) error {
	enc := json.NewEncoder(w)
	if opts.Indent {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	return enc.Encode(jsonEnvelope{Schema: SchemaVersion, Result: WithPreviews(res, opts.Previews)})
}

// WithPreviews returns a copy of res whose questions section carries exactly the
// given previews: none at all when previews is nil, which also strips any a
// loaded report carried. res itself is never modified: the Result struct, the
// Questions struct it points to and the Items slice are all copied before a
// Preview is written, because callers share res (the server's cache, overview
// inputs). Every JSON and HTML writer goes through it, which is what keeps the
// store and the default files free of prompt text.
func WithPreviews(res *analyzer.Result, previews map[int]string) *analyzer.Result {
	if res == nil || res.Questions == nil {
		return res
	}
	out := *res
	q := *res.Questions
	q.Items = make([]analyzer.Question, len(res.Questions.Items))
	copy(q.Items, res.Questions.Items)
	q.PreviewsIncluded = previews != nil
	for i := range q.Items {
		q.Items[i].Preview = ""
		if previews != nil && q.Items[i].Number > 0 {
			q.Items[i].Preview = previews[q.Items[i].Number]
		}
	}
	out.Questions = &q
	return &out
}

// PreviewsOf collects the previews a loaded report already carries (report --from).
// It returns nil when the report carries none, so passing the result on to
// WithPreviews strips rather than marks an empty set as included.
func PreviewsOf(res *analyzer.Result) map[int]string {
	if res == nil || res.Questions == nil || !res.Questions.PreviewsIncluded {
		return nil
	}
	out := map[int]string{}
	for _, it := range res.Questions.Items {
		if it.Number > 0 {
			out[it.Number] = it.Preview
		}
	}
	return out
}
