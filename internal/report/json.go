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

// WriteJSON emits the machine-readable report.
//
// The analysis struct deliberately excludes the parsed trace, so no prompt text,
// file content or command output can leak into the JSON. Only counts, hashes,
// paths and short redacted evidence excerpts are serialized.
func WriteJSON(w io.Writer, res *analyzer.Result, indent bool) error {
	enc := json.NewEncoder(w)
	if indent {
		enc.SetIndent("", "  ")
	}
	enc.SetEscapeHTML(false)
	return enc.Encode(jsonEnvelope{Schema: SchemaVersion, Result: res})
}
