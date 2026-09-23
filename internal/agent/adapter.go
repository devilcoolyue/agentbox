package agent

import (
	"encoding/json"
	"fmt"

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
}
type Adapter interface {
	Type() string
	Capabilities() Capabilities
	Chat(permission, resume, model, effort string) ([]string, error)
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
func (a cliAdapter) Chat(permission, resume, model, effort string) ([]string, error) {
	return ChatCommand(string(a), permission, resume, model, effort)
}
func (a cliAdapter) Title() ([]string, error)               { return TitleCommand(string(a)) }
func (a cliAdapter) ParseTitle(out string) (string, []byte) { return TitleOutput(string(a), out) }
func (a cliAdapter) Decode(line []byte) (Event, bool) {
	var probe struct {
		Type string `json:"type"`
	}
	if len(line) == 0 || line[0] != '{' || json.Unmarshal(line, &probe) != nil {
		return Event{}, false
	}
	e := Event{Raw: append(json.RawMessage(nil), line...), Partial: IsPartialEvent(line), SessionID: ExtractSessionID(line)}
	switch probe.Type {
	case "stream_event", "assistant", "item.started", "item.completed":
		e.Output = true
	}
	return e, true
}
