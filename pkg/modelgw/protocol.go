// Package modelgw forwards model API requests to a fixed or per-request
// upstream and decodes the four supported SSE protocols into incremental text
// deltas. Inspectors decide whether a request may run and whether a stream must
// be interrupted; the gateway alone performs the cancellation and writes the
// protocol-correct terminal error, so an inspector can never fabricate success.
package modelgw

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Protocol identifies a streaming model API by its endpoint path.
type Protocol string

const (
	Responses Protocol = "responses"
	Chat      Protocol = "chat"
	Anthropic Protocol = "anthropic"
	Gemini    Protocol = "gemini"
)

// ProtocolFor maps a request path to its protocol, or "" when the endpoint is
// not inspected.
func ProtocolFor(path string) Protocol {
	switch {
	case strings.HasSuffix(path, "/responses"):
		return Responses
	case strings.HasSuffix(path, "/chat/completions"):
		return Chat
	case strings.HasSuffix(path, "/messages"):
		return Anthropic
	case strings.HasSuffix(path, ":streamGenerateContent"):
		return Gemini
	}
	return ""
}

// Delta is one decoded text increment. Part is an opaque partition key
// (candidate, output item, content block, summary index); consumers must keep
// independent state per (Part, Channel). Channel is "text" or "reasoning".
type Delta struct {
	Part    string `json:"part"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

// Decoder follows one response stream. It reads only incremental text fields,
// never tool arguments, tool results, usage, or the full snapshots that
// accompany completion events. It also remembers what Failure needs to write a
// valid terminal error for the protocol.
type Decoder struct {
	protocol Protocol
	domain   string
	sequence int64
	response map[string]any
}

func NewDecoder(p Protocol) *Decoder { return &Decoder{protocol: p, domain: "loopguard"} }

// WithDomain sets the google.rpc.ErrorInfo domain used by Gemini failures.
func (d *Decoder) WithDomain(domain string) *Decoder {
	if domain != "" {
		d.domain = domain
	}
	return d
}

func (d *Decoder) Protocol() Protocol { return d.protocol }

// Observe decodes one raw SSE event (including its trailing blank line). ended
// lists parts that closed in this event; it is reported before the deltas.
func (d *Decoder) Observe(event []byte) (deltas []Delta, ended []string) {
	var data []string
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	var v map[string]any
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &v) != nil {
		return nil, nil
	}
	add := func(part, channel, text string) {
		if text != "" {
			deltas = append(deltas, Delta{Part: part, Channel: channel, Text: text})
		}
	}
	switch d.protocol {
	case Responses:
		t := str(v["type"])
		if t == "response.created" || t == "response.in_progress" {
			r := obj(v["response"])
			d.response = make(map[string]any)
			for _, k := range []string{"id", "object", "created_at", "model"} {
				if value, ok := r[k]; ok {
					d.response[k] = value
				}
			}
		}
		if n, ok := v["sequence_number"].(float64); ok && n > float64(d.sequence) && n < 1<<53 {
			d.sequence = int64(n)
		}
		key := fmt.Sprint(v["output_index"], ":", v["item_id"], ":", v["content_index"], ":", v["summary_index"])
		switch t {
		case "response.output_text.delta":
			add(key, "text", str(v["delta"]))
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			add(key+":"+t, "reasoning", str(v["delta"]))
		}
	case Anthropic:
		key := fmt.Sprint(v["index"])
		switch str(v["type"]) {
		case "content_block_stop":
			ended = append(ended, key)
		case "content_block_delta":
			delta := obj(v["delta"])
			switch str(delta["type"]) {
			case "text_delta":
				add(key, "text", str(delta["text"]))
			case "thinking_delta":
				add(key, "reasoning", str(delta["thinking"]))
			}
		}
	case Chat:
		for i, raw := range arr(v["choices"]) {
			c := obj(raw)
			key := strconv.Itoa(i)
			if c["index"] != nil {
				key = fmt.Sprint(c["index"])
			}
			delta := obj(c["delta"])
			think := str(delta["reasoning_content"])
			if think == "" {
				think = str(delta["reasoning"])
			}
			add(key, "reasoning", think)
			add(key, "text", str(delta["content"]))
		}
	case Gemini:
		for i, raw := range arr(v["candidates"]) {
			c := obj(raw)
			key := strconv.Itoa(i)
			if c["index"] != nil {
				key = fmt.Sprint(c["index"])
			}
			for _, part := range arr(obj(c["content"])["parts"]) {
				p := obj(part)
				channel := "text"
				if p["thought"] == true {
					channel = "reasoning"
				}
				add(key, channel, str(p["text"]))
			}
		}
	}
	return deltas, ended
}

func str(v any) string         { s, _ := v.(string); return s }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }

// Failure renders the terminal error events that end an interrupted stream.
// It never emits a success event and never replaces content already sent.
func (d *Decoder) Failure(code, message string) []byte {
	e := map[string]any{"type": code, "code": code, "message": message}
	var payload any = map[string]any{"error": e}
	switch d.protocol {
	case Responses:
		// Use the documented error event. A synthetic response.failed with
		// an empty output snapshot could erase previously delivered content.
		payload = map[string]any{"type": "error", "code": code, "message": message, "param": nil, "sequence_number": d.sequence + 1}
	case Anthropic:
		e["type"] = "api_error"
		payload = map[string]any{"type": "error", "error": e}
	case Gemini:
		payload = map[string]any{"error": map[string]any{"code": 400, "status": "FAILED_PRECONDITION", "message": code + ": " + message,
			"details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": strings.ToUpper(code), "domain": d.domain}}}}
	}
	b, _ := json.Marshal(payload)
	if d.protocol == Responses {
		// Codex 0.162.1 ignores the generic error event and waits for a
		// response terminal event. Send failed as well, with no replacement
		// output snapshot: previously forwarded items remain authoritative.
		r := make(map[string]any, len(d.response)+3)
		for k, v := range d.response {
			r[k] = v
		}
		r["object"], r["status"], r["error"] = "response", "failed", e
		failed, _ := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": d.sequence + 2, "response": r})
		return []byte("event: error\ndata: " + string(b) + "\n\nevent: response.failed\ndata: " + string(failed) + "\n\n")
	}
	if d.protocol == Chat || d.protocol == Gemini {
		return []byte("data: " + string(b) + "\n\n")
	}
	return []byte("event: error\ndata: " + string(b) + "\n\n")
}
