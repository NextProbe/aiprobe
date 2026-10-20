package trace

import "time"

// Origin says who wrote a user-role message, or a record that stands for a
// command the person issued. Adapters set it, because only an adapter can read
// the runtime's own markers; the analyzer opens a question on Prompt, Command and
// Shell and never inspects runtime-specific text to decide.
type Origin string

const (
	// OriginUnclassified is the zero value: an event the adapter did not
	// classify. The analyzer treats it as "does not open a question".
	OriginUnclassified Origin = ""
	// OriginPrompt is text the person typed, including a message that carried
	// only attachments.
	OriginPrompt Origin = "prompt"
	// OriginCommand is a slash command the person issued.
	OriginCommand Origin = "command"
	// OriginShell is a shell-mode command the person issued (Claude Code "!").
	OriginShell Origin = "shell"
	// OriginInjected is content the runtime, a hook, steering or a workflow wrote
	// into a user role: command bodies and output, reminders, notifications,
	// compaction summaries, interruption markers.
	OriginInjected Origin = "injected"
	// OriginPreamble is a wrapper the runtime writes in front of every message
	// the person sends (Claude Code's command caveat, Codex's per-turn
	// environment context). It never opens a question and is not counted as an
	// injected message, because it says nothing about the question it precedes.
	OriginPreamble Origin = "preamble"
)

// OpensQuestion reports whether an event with this origin starts a question.
func (o Origin) OpensQuestion() bool {
	return o == OriginPrompt || o == OriginCommand || o == OriginShell
}

// Prompts is the adapter's statement about whether it classified user-role events.
type Prompts string

const (
	// PromptsUnclassified: the adapter does not set Origin. The analyzer reports
	// the question split as unavailable rather than guessing.
	PromptsUnclassified Prompts = ""
	// PromptsClassified: every user-role event carries an Origin.
	PromptsClassified Prompts = "classified"
	// PromptsMissing: what was read does not contain the person's prompts at all
	// (Kiro CLI rebuilt from its sidecar).
	PromptsMissing Prompts = "missing"
)

// TurnStatus is how the runtime says a turn ended. Set only when the runtime
// recorded it; never inferred.
type TurnStatus string

const (
	TurnCompleted   TurnStatus = ""
	TurnInterrupted TurnStatus = "interrupted"
	TurnCancelled   TurnStatus = "cancelled"
	TurnFailed      TurnStatus = "error"
)

// RuntimeTurn is one turn as the runtime itself delimits it: a Kiro
// turn_start/turn_end pair, a Kiro CLI user turn, a Codex task. A question can
// span several, for example when an injected message wakes the agent.
type RuntimeTurn struct {
	// Start and End bound the events the turn covered, inclusive. End < Start for
	// a turn that recorded no events. Start is -1 when the adapter could not
	// place the turn in the timeline at all; any token counters the adapter
	// attached for such a turn sit on a request it chose by position, which the
	// analyzer reports as an incomplete split rather than as measured.
	Start int
	End   int
	// Status is how the turn ended, when the runtime said.
	Status TurnStatus
	// Charge is the runtime's own charge for the turn, in Trace.CostUnit. Unknown
	// when the runtime reported none for this turn.
	Charge OptFloat
	// EndTime and Duration are the runtime's own figures, zero when absent.
	EndTime  time.Time
	Duration time.Duration
	// ReportedRequests is the runtime's own count of provider calls. Display
	// only; never reconciled against Trace.Requests.
	ReportedRequests int
}

// UsageClass names one of the four billed token classes.
type UsageClass string

const (
	UsageInput      UsageClass = "input"
	UsageOutput     UsageClass = "output"
	UsageCacheRead  UsageClass = "cache_read"
	UsageCacheWrite UsageClass = "cache_write"
)
