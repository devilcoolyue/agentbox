package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

// SlashCommand is one command the container's Claude Code offers, as its
// initialize control response lists it: built-ins, skills, plugin and
// project commands. The web composer suggests them after "/".
type SlashCommand struct {
	Name         string   `json:"name"`
	Description  string   `json:"description,omitempty"`
	ArgumentHint string   `json:"argument_hint,omitempty"`
	Aliases      []string `json:"aliases,omitempty"`
	Builtin      bool     `json:"builtin,omitempty"`
}

const (
	commandsRequestID   = "agentbox-commands"
	maxSlashCommands    = 500
	maxCommandAliases   = 8
	maxCommandNameRunes = 128
	maxCommandDescRunes = 1000
	maxCommandHintRunes = 200
)

// ClaudeCommandsCommand starts a CLI that only answers initialize: no prompt
// is ever written, so the model is not called and, with persistence off,
// nothing is saved. A strict, empty MCP config keeps a listing from starting
// the user's MCP servers; their prompts are left out of the suggestions.
func ClaudeCommandsCommand() []string {
	return []string{"claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json",
		"--verbose", "--no-session-persistence", "--strict-mcp-config"}
}

// ClaudeCommandsFlags are the --help entries ClaudeCommandsCommand relies on.
var ClaudeCommandsFlags = []string{"--input-format", "--no-session-persistence", "--strict-mcp-config"}

// ProbeClaudeCommands sends initialize and returns the CLI's commands once
// its response arrives. The caller owns cancellation and closing.
func ProbeClaudeCommands(w io.Writer, r io.Reader) ([]SlashCommand, error) {
	req, _ := json.Marshal(map[string]any{"type": "control_request", "request_id": commandsRequestID,
		"request": map[string]string{"subtype": "initialize"}})
	if _, err := w.Write(append(req, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(r)
	// The response also carries models, agents and account details.
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.Contains(line, []byte(`"control_response"`)) {
			continue
		}
		var msg struct {
			Type     string `json:"type"`
			Response struct {
				Subtype   string `json:"subtype"`
				RequestID string `json:"request_id"`
				Error     string `json:"error"`
				Response  struct {
					Commands []struct {
						Name         string   `json:"name"`
						Description  string   `json:"description"`
						ArgumentHint string   `json:"argumentHint"`
						Aliases      []string `json:"aliases"`
						Builtin      bool     `json:"builtin"`
					} `json:"commands"`
				} `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(line, &msg) != nil || msg.Type != "control_response" || msg.Response.RequestID != commandsRequestID {
			continue
		}
		if msg.Response.Subtype != "success" {
			return nil, fmt.Errorf("initialize refused: %s", oneLine(msg.Response.Error, 200))
		}
		out := make([]SlashCommand, 0, len(msg.Response.Response.Commands))
		seen := map[string]bool{}
		for _, c := range msg.Response.Response.Commands {
			if len(out) == maxSlashCommands {
				break
			}
			if seen[c.Name] || !validCommandName(c.Name) {
				continue
			}
			seen[c.Name] = true
			cmd := SlashCommand{Name: c.Name, Description: oneLine(c.Description, maxCommandDescRunes),
				ArgumentHint: oneLine(c.ArgumentHint, maxCommandHintRunes), Builtin: c.Builtin}
			for _, alias := range c.Aliases {
				if len(cmd.Aliases) < maxCommandAliases && validCommandName(alias) {
					cmd.Aliases = append(cmd.Aliases, alias)
				}
			}
			out = append(out, cmd)
		}
		return out, nil
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("initialize: CLI exited without a response")
}

func validCommandName(name string) bool {
	return utf8.RuneCountInString(name) <= maxCommandNameRunes && claudeCommandName.MatchString(name)
}

// oneLine keeps CLI-provided text to one plain line of bounded length.
func oneLine(s string, limit int) string {
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	return string([]rune(s)[:limit]) + "…"
}
