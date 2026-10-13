package delegation

import (
	"fmt"
	"strings"

	"agentbox/internal/config"
)

// PromptVersion identifies the wording below in recorded routes and docs.
const PromptVersion = "delegation-prompt-v3"

// Tool names as the main agent sees them (server "agentbox" in --mcp-config).
const (
	DelegateTool = "mcp__agentbox__delegate"
	FeedbackTool = "mcp__agentbox__delegation_feedback"
)

// MainPrompt is appended to the main agent's system prompt for one turn.
func MainPrompt(workers []Worker) string {
	var b strings.Builder
	b.WriteString("# Delegation to worker models (agentbox)\n\n")
	b.WriteString("The user turned on delegation for this message. They want you, the expensive main model, to spend your effort on judgement: ")
	b.WriteString("planning, decisions, the critical code and reviewing results. The legwork (reading and searching code, routine edits, tests) should go to cheaper worker models through the tool " + DelegateTool + ". ")
	b.WriteString("You stay responsible for the final answer. Workers run in the same /workspace and edit the same files directly. They cannot see this conversation, so every task must be self-contained.\n\n")
	b.WriteString("Workers offered this turn (the platform picks among them per call):\n")
	for _, w := range workers {
		fmt.Fprintf(&b, "- %s (%s, %s tier): can %s", w.Label(), w.Agent, w.Tags.Tier, strings.Join(w.Tags.Can, ", "))
		if len(w.Tags.Avoid) > 0 {
			fmt.Fprintf(&b, "; avoids %s", strings.Join(w.Tags.Avoid, ", "))
		}
		if w.Tags.Notes != "" {
			fmt.Fprintf(&b, ". %s", w.Tags.Notes)
		}
		b.WriteString("\n")
	}
	b.WriteString(`
Default to delegating. Before you start reading files or running commands yourself, decide which parts of the request a worker above can take, and delegate those first:
- Understanding code (what a project does, how a feature works, where something is handled): delegate a search or summarize task with precise questions. Ask for file paths with line numbers and short quotes of the key lines, so you can spot-check a few claims instead of reading everything.
- Changes that follow a plan you made: delegate implementing, bulk edits, tests and test fixes, each as one well-specified task.
Do the work yourself only when it is one of these:
- Judgement: architecture or interface decisions, resolving ambiguous requirements, and changes with risks the workers avoid (the platform also declines those).
- Trivial: a single command or one small file you already know exactly.
- A task type no offered worker can take.
Reading a few files to plan, or to check a worker's answer, is fine. Reading a whole module yourself instead of delegating is not what the user asked for.

How to delegate:
- task: goal, relevant files or where to look, interfaces and conventions to follow, acceptance criteria, what must not be touched, and what the report must contain.
- task_type: exactly one of ` + strings.Join(config.DelegationTaskTypes, ", ") + `.
- risks: every one of ` + strings.Join(config.DelegationRisks, ", ") + ` that applies; an empty list if none.
- access: "read" for investigation only, "write" when files may change.
- The platform routes the call. "declined" means it should not go to a worker: do that work yourself and do not resubmit it in other words.
- After a write delegation, read the actual diff of changed_files before relying on it. Fix small problems yourself, or call delegate again with continue set to the delegation_id and concrete feedback.
- After reviewing each finished delegation, call ` + FeedbackTool + ` with verdict ok, minor_fix, redo or abandoned.
- One delegation runs at a time, at most ` + fmt.Sprint(MaxCallsPerTurn) + ` per turn. Treat a worker's claims about tests as unverified unless you saw the output.
- Reply to the user in the user's language and say which parts were delegated and to which worker.
`)
	return b.String()
}

// TurnReminder precedes the user's message in a delegating turn. The system
// prompt alone was not enough: in a resumed conversation whose earlier turns
// were answered without workers, the main agent kept reading code itself.
// Claude Code also defers MCP tool schemas, so the reminder names the
// ToolSearch query that loads them.
func TurnReminder() string {
	return "<agentbox-delegation>\n" +
		"Delegation is ON for this message: the user switched it on and expects workers to do the legwork. " +
		"Earlier turns of this conversation may have been answered without workers; that does not apply to this message.\n" +
		"Before you read files or run commands yourself, hand the reading, searching and routine work to a worker with " + DelegateTool +
		", following the delegation guidance in your system prompt. If the tool's schema is not loaded yet, first call ToolSearch with query \"select:" +
		DelegateTool + "," + FeedbackTool + "\".\n" +
		"</agentbox-delegation>"
}

// WorkerPrompt instructs a worker CLI. It is appended to Claude Code's system
// prompt, or prepended to the task for Codex.
func WorkerPrompt(t Task) string {
	var b strings.Builder
	b.WriteString("You are a worker agent. A main agent delegated one subtask to you through agentbox. ")
	b.WriteString("Do exactly this subtask in /workspace and do not expand its scope. ")
	b.WriteString("If something is unclear or blocked, stop and explain it in your report instead of guessing. ")
	b.WriteString("Do not commit, push, deploy or delete unrelated files.")
	if t.Access == "read" {
		b.WriteString(" This is a read-only subtask: do not modify any files. Support each finding with file paths and line numbers, quoting the key lines briefly.")
	}
	b.WriteString("\n\nWhen you finish, reply with a short report under these headings, written in the language of the task:\n")
	b.WriteString("## Done\n## Commands and results\n## Open issues\n")
	return b.String()
}

// WorkerInput is the user message the worker receives.
func WorkerInput(t Task, feedback bool) string {
	var b strings.Builder
	if feedback {
		b.WriteString("Follow-up from the main agent on your previous work:\n\n")
	} else {
		fmt.Fprintf(&b, "Subtask type: %s\n", t.Type)
		if len(t.Risks) > 0 {
			fmt.Fprintf(&b, "Known risks: %s\n", strings.Join(t.Risks, ", "))
		}
		fmt.Fprintf(&b, "Access: %s\n\n", t.Access)
	}
	b.WriteString(t.Text)
	return b.String()
}
