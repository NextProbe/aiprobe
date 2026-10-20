package claude

import (
	"encoding/json"
	"regexp"
	"strings"

	"github.com/NextProbe/aiprobe/internal/trace"
)

// Who wrote a user-role message.
//
// Claude Code writes a great deal into the user role that the person never
// typed: slash-command expansions and their output, reminders, compaction
// summaries, interruption markers. A question starts only where the person
// spoke, so every user message is classified here, by the rules below, in
// order; the first that matches decides. The analyzer never looks at the text.

// reWrappers matches the blocks Claude Code wraps around a person's message.
var reWrappers = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>|<ide_opened_file>.*?</ide_opened_file>|<ide_selection>.*?</ide_selection>`)

// interruptMarker starts the line Claude Code writes when the person stops a
// response ("[Request interrupted by user]", "... for tool use]").
const interruptMarker = "[Request interrupted by user"

// unwrap removes the wrappers and surrounding space, leaving what the person wrote.
func unwrap(text string) string {
	if strings.Contains(text, "<") {
		text = reWrappers.ReplaceAllString(text, "")
	}
	return strings.TrimSpace(text)
}

// dropInterruptions removes interruption-marker lines, so a new message written
// into the same record as the marker still reads as what the person typed.
func dropInterruptions(text string) string {
	if !strings.Contains(text, interruptMarker) {
		return text
	}
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), interruptMarker) {
			continue
		}
		kept = append(kept, l)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// interrupted reports whether text carries an interruption-marker line.
func interrupted(text string) bool {
	if !strings.Contains(text, interruptMarker) {
		return false
	}
	for _, l := range strings.Split(unwrap(text), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), interruptMarker) {
			return true
		}
	}
	return false
}

// classifyText applies the text rules (3 to 11) to one user message. Rules 1 and 2
// (a record carrying tool results, a sidechain record) depend on the record and
// are applied by the caller. media is true when the message carried image or
// document blocks, which make a message with no text a prompt rather than noise.
func classifyText(text string, isMeta, compacted, media bool) (trace.Origin, string) {
	bare := unwrap(text)
	switch {
	case isMeta && strings.HasPrefix(bare, "<local-command-caveat>"):
		// Written in front of every slash command; it says nothing about the
		// command, so it is neither a question nor an injected message.
		return trace.OriginPreamble, ""
	case isMeta:
		// Command bodies, image placeholders, retry nudges, skill text.
		return trace.OriginInjected, ""
	case compacted:
		return trace.OriginInjected, ""
	case bare == "" && !media:
		// Nothing but reminders.
		return trace.OriginInjected, ""
	}
	rest := dropInterruptions(bare)
	switch {
	case rest == "" && !media:
		// A bare interruption marker.
		return trace.OriginInjected, ""
	case hasPrefix(rest, "<local-command-stdout>", "<local-command-stderr>", "<bash-stdout>", "<bash-stderr>"):
		return trace.OriginInjected, ""
	case hasPrefix(rest, "<command-name>", "<command-message>"):
		// Only at the start: a real prompt may well quote the tag, for example
		// when it discusses this very transcript format.
		return trace.OriginCommand, commandDisplay(rest)
	case strings.HasPrefix(rest, "<bash-input>"):
		return trace.OriginShell, "!" + strings.TrimSpace(tagText(rest, "bash-input"))
	}
	return trace.OriginPrompt, rest
}

// commandDisplay renders a slash command as the person typed it: "/name args".
func commandDisplay(text string) string {
	name := strings.TrimSpace(tagText(text, "command-name"))
	if name == "" {
		name = strings.TrimSpace(tagText(text, "command-message"))
	}
	if !strings.HasPrefix(name, "/") {
		name = "/" + name
	}
	if args := strings.TrimSpace(tagText(text, "command-args")); args != "" {
		name += " " + args
	}
	return name
}

// tagText returns the content of the first <tag>…</tag> in text, or "".
func tagText(text, tag string) string {
	open, close := "<"+tag+">", "</"+tag+">"
	i := strings.Index(text, open)
	if i < 0 {
		return ""
	}
	rest := text[i+len(open):]
	if j := strings.Index(rest, close); j >= 0 {
		return rest[:j]
	}
	return rest
}

func hasPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// startsCommand reports whether a record's text is an old-format slash command,
// which earlier Claude Code versions wrote as a system record.
func startsCommand(text string) bool {
	return hasPrefix(strings.TrimSpace(text), "<command-name>", "<command-message>")
}

// queuedPrompt reads a queued_command attachment: a message the person typed
// while the agent was working, absorbed into the running turn. ok is false for
// any other attachment, or a shape this adapter does not recognise, which then
// keeps the generic unknown-record handling.
func queuedPrompt(raw json.RawMessage) (text string, origin trace.Origin, display string, ok bool) {
	var a rawAttachment
	if len(raw) == 0 || json.Unmarshal(raw, &a) != nil {
		return "", "", "", false
	}
	if stringOf(a.Type) != "queued_command" {
		return "", "", "", false
	}
	text, ok = attachmentText(a.Prompt)
	if !ok {
		return "", "", "", false
	}
	switch mode := stringOf(a.CommandMode); {
	case mode == "prompt" && humanOrigin(a.Origin):
		return text, trace.OriginPrompt, text, true
	case mode == "bash":
		return text, trace.OriginShell, "!" + text, true
	}
	// Task notifications and anything else the runtime queued on its own behalf.
	return text, trace.OriginInjected, "", true
}

// attachmentText reads a queued prompt: a string, or content blocks of which only
// the text blocks count.
func attachmentText(raw json.RawMessage) (string, bool) {
	if s := stringOf(raw); s != "" {
		return s, true
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '[' {
		return "", false
	}
	blocks, plain := decodeBlocks(raw)
	if blocks == nil {
		return plain, plain != ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			continue
		}
		if t := blockText(b); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n"), true
}

// humanOrigin reports whether a queued message came from the person. A missing
// origin counts as human, which is what every observed prompt-mode entry is.
func humanOrigin(raw json.RawMessage) bool {
	if !marked(raw) {
		return true
	}
	var o struct {
		Kind json.RawMessage `json:"kind"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return true
	}
	kind := stringOf(o.Kind)
	return kind == "" || kind == "human"
}

// stringOf returns raw as a string when it is a JSON string, and "" for any
// other shape. A field that changed shape reads as absent; it never costs the
// line it sits on.
func stringOf(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed[0] != '"' {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// marked reports whether a marker field is set: present, not null and not "".
func marked(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	return trimmed != "" && trimmed != "null" && trimmed != `""`
}
