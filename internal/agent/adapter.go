package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"agentbox/internal/config"
)

// Capabilities describes supported transport/telemetry, not model availability.
type Capabilities struct {
	AppServer     bool
	TerminalUsage bool
}
type Event struct {
	Raw       json.RawMessage
	Partial   bool
	Output    bool
	SessionID string
	// Terminal is a provider-confirmed outcome, not a socket/exit-code guess.
	Terminal string
	// Local marks a Claude result for a slash command the CLI ran itself
	// (num_turns 0): /context or an unknown command never asks the model,
	// /compact makes its own summary request, which the usage meter bills
	// from the CLI's cost-state. Either way the turn says nothing worth
	// naming a thread by.
	Local bool
}
type Adapter interface {
	Type() string
	Capabilities() Capabilities
	Chat(permission, resume, model, effort string, control ...string) ([]string, error)
	ChatWith(extras ChatExtras, permission, resume, model, effort, control string) ([]string, error)
	Title() ([]string, error)
	ParseTitle(string) (string, []byte)
	Decode([]byte) (Event, bool)
}
type cliAdapter string

func Lookup(kind string) (Adapter, error) {
	switch kind {
	case config.AgentClaude, config.AgentCodex:
		return cliAdapter(kind), nil
	}
	return nil, fmt.Errorf("unsupported agent %q", kind)
}
func (a cliAdapter) Type() string { return string(a) }
func (a cliAdapter) Capabilities() Capabilities {
	return Capabilities{AppServer: string(a) == config.AgentCodex, TerminalUsage: true}
}
func (a cliAdapter) Chat(permission, resume, model, effort string, control ...string) ([]string, error) {
	return ChatCommand(string(a), permission, resume, model, effort, control...)
}
func (a cliAdapter) ChatWith(extras ChatExtras, permission, resume, model, effort, control string) ([]string, error) {
	return ChatCommandWith(extras, string(a), permission, resume, model, effort, control)
}
func (a cliAdapter) Title() ([]string, error)               { return TitleCommand(string(a)) }
func (a cliAdapter) ParseTitle(out string) (string, []byte) { return TitleOutput(string(a), out) }
func (a cliAdapter) Decode(line []byte) (Event, bool) {
	var probe struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
		IsError bool   `json:"is_error"`
		Status  string `json:"status"`
		// Claude result: model round-trips in the turn, 0 for a local command.
		NumTurns *int `json:"num_turns"`
	}
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &probe) != nil {
		return Event{}, false
	}
	e := Event{Raw: append(json.RawMessage(nil), line...), Partial: IsPartialEvent(line), SessionID: ExtractSessionID(line)}
	switch probe.Type {
	case "stream_event", "assistant", "item.started", "item.completed":
		e.Output = true
	}
	if string(a) == config.AgentClaude && probe.Type == "result" {
		if probe.Subtype == "success" && !probe.IsError {
			e.Terminal = "completed"
			e.Local = probe.NumTurns != nil && *probe.NumTurns == 0
		} else if probe.IsError || strings.HasPrefix(probe.Subtype, "error_") {
			e.Terminal = "failed"
		}
	}
	if string(a) == config.AgentCodex {
		switch probe.Type {
		case "turn.completed":
			e.Terminal = "completed"
		case "turn.failed":
			e.Terminal = "failed"
			if probe.Status == "interrupted" {
				e.Terminal = "interrupted"
			}
		}
	}
	return e, true
}
