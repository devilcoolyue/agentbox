package agent

import (
	"errors"

	"agentbox/internal/config"
)

// KeepalivePidFile keeps a cache keepalive's CLI apart from the turn's: the
// user's stop signals PidFile and must never reach it, nor it the turn.
const KeepalivePidFile = "/tmp/.agentbox-keepalive.pid"

// KeepalivePrompt is the throwaway message of a keepalive request. The CLI
// saves nothing of the exchange (see KeepaliveCommand), so it never shows up
// in the conversation.
const KeepalivePrompt = "Reply with just OK. Do not use any tools."

// KeepaliveDiscovery keeps a keepalive from loading instruction files; see
// KeepaliveCommand.
const KeepaliveDiscovery = "CLAUDE_CODE_DISABLE_CLAUDE_MDS=1"

// KeepaliveCommand resumes a Claude conversation for one request that reads
// its prompt cache, which restarts the cache's lifetime. The request must
// repeat the prefix the turn cached, so model, reasoning and the task-tool
// switch are that turn's; the input format, prompt suggestions and permission
// mode leave the cached prefix alone (measured against 2.1.303) and stay out.
// --no-session-persistence keeps the exchange out of the transcript,
// --max-turns 1 stops after one response, and the default permission mode
// refuses in -p any tool that needs approval. dir is the working directory
// the conversation ran in when it is not /workspace (a terminal session):
// the CLI finds the session by it. A resumed session repeats the system
// prompt and tools its transcript recorded, so a session the terminal's
// interactive CLI started keeps its prefix as well (measured against 2.1.303).
//
// A process resuming a session whose instruction files (CLAUDE.md and what it
// pulls in) changed since the session recorded them attaches them all again
// after the conversation. The terminal's long-lived CLI never sends that, so
// the keepalive would write it into the cache for nothing — 74K tokens for
// this repository's AGENTS.md. KeepaliveDiscovery turns the discovery off:
// the recorded files then read as gone, a few lines, and the prefix is the
// same (measured against 2.1.303: 6,274 tokens written without it, 327 with).
func KeepaliveCommand(conversation, dir, model, effort, control string, taskTools bool) ([]string, error) {
	if conversation == "" {
		return nil, errors.New("keepalive needs a conversation to resume")
	}
	extras := ChatExtras{PidFile: KeepalivePidFile, TaskTools: taskTools, Dir: dir, Env: []string{KeepaliveDiscovery}}
	cmd, err := ChatCommandWith(extras, config.AgentClaude, "default", conversation, model, effort, control)
	if err != nil {
		return nil, err
	}
	return append(cmd, "--max-turns", "1", "--no-session-persistence"), nil
}
