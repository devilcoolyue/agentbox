package loopguard

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

const ErrorCode = "model_output_loop"

type protocol string

const (
	responses protocol = "responses"
	chat      protocol = "chat"
	anthropic protocol = "anthropic"
	gemini    protocol = "gemini"
)

func protocolFor(path string) protocol {
	switch {
	case strings.HasSuffix(path, "/responses"):
		return responses
	case strings.HasSuffix(path, "/chat/completions"):
		return chat
	case strings.HasSuffix(path, "/messages"):
		return anthropic
	case strings.HasSuffix(path, ":streamGenerateContent"):
		return gemini
	}
	return ""
}

type streamGuard struct {
	protocol protocol
	streams  map[string]*Detector
	sequence int64
	response map[string]any
}

func (g *streamGuard) push(key, channel, text string) *Detection {
	if text == "" {
		return nil
	}
	if g.streams == nil {
		g.streams = make(map[string]*Detector)
	}
	d := g.streams[key+":"+channel]
	if d == nil {
		// Bound exotic upstreams with unbounded item IDs. Eviction only loses
		// detection history; it cannot combine unrelated candidates.
		if len(g.streams) >= 128 {
			clear(g.streams)
		}
		d = &Detector{}
		g.streams[key+":"+channel] = d
	}
	if hit := d.Push(text); hit != nil {
		result := *hit
		result.Channel = channel
		return &result
	}
	return nil
}

// observe reads only incremental text fields, never tool arguments, tool
// results, usage, or the full snapshots accompanying completion events.
func (g *streamGuard) observe(event []byte) *Detection {
	var data []string
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	var v map[string]any
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &v) != nil {
		return nil
	}
	switch g.protocol {
	case responses:
		t := str(v["type"])
		if t == "response.created" || t == "response.in_progress" {
			r := obj(v["response"])
			g.response = make(map[string]any)
			for _, k := range []string{"id", "object", "created_at", "model"} {
				if value, ok := r[k]; ok {
					g.response[k] = value
				}
			}
		}
		if n, ok := v["sequence_number"].(float64); ok && n > float64(g.sequence) && n < 1<<53 {
			g.sequence = int64(n)
		}
		key := fmt.Sprint(v["output_index"], ":", v["item_id"], ":", v["content_index"], ":", v["summary_index"])
		switch t {
		case "response.output_text.delta":
			return g.push(key, "text", str(v["delta"]))
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			return g.push(key+":"+t, "reasoning", str(v["delta"]))
		}
	case anthropic:
		key := fmt.Sprint(v["index"])
		if str(v["type"]) == "content_block_stop" {
			delete(g.streams, key+":text")
			delete(g.streams, key+":reasoning")
		}
		if str(v["type"]) == "content_block_delta" {
			d := obj(v["delta"])
			switch str(d["type"]) {
			case "text_delta":
				return g.push(key, "text", str(d["text"]))
			case "thinking_delta":
				return g.push(key, "reasoning", str(d["thinking"]))
			}
		}
	case chat:
		for i, raw := range arr(v["choices"]) {
			c := obj(raw)
			key := strconv.Itoa(i)
			if c["index"] != nil {
				key = fmt.Sprint(c["index"])
			}
			d := obj(c["delta"])
			think := str(d["reasoning_content"])
			if think == "" {
				think = str(d["reasoning"])
			}
			if hit := g.push(key, "reasoning", think); hit != nil {
				return hit
			}
			if hit := g.push(key, "text", str(d["content"])); hit != nil {
				return hit
			}
		}
	case gemini:
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
				if hit := g.push(key, channel, str(p["text"])); hit != nil {
					return hit
				}
			}
		}
	}
	return nil
}

func str(v any) string         { s, _ := v.(string); return s }
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func arr(v any) []any          { a, _ := v.([]any); return a }

func (g *streamGuard) failure(code, message string) []byte {
	e := map[string]any{"type": code, "code": code, "message": message}
	name := "error"
	var payload any = map[string]any{"error": e}
	switch g.protocol {
	case responses:
		// Use the documented error event. A synthetic response.failed with
		// an empty output snapshot could erase previously delivered content.
		payload = map[string]any{"type": "error", "code": code, "message": message, "param": nil, "sequence_number": g.sequence + 1}
	case anthropic:
		e["type"] = "api_error"
		payload = map[string]any{"type": "error", "error": e}
	case gemini:
		payload = map[string]any{"error": map[string]any{"code": 400, "status": "FAILED_PRECONDITION", "message": code + ": " + message,
			"details": []any{map[string]any{"@type": "type.googleapis.com/google.rpc.ErrorInfo", "reason": strings.ToUpper(code), "domain": "loopguard"}}}}
	}
	b, _ := json.Marshal(payload)
	if g.protocol == responses {
		// Codex 0.162.1 ignores the generic error event and waits for a
		// response terminal event. Send failed as well, with no replacement
		// output snapshot: previously forwarded items remain authoritative.
		r := g.response
		if r == nil {
			r = make(map[string]any)
		}
		r["object"], r["status"], r["error"] = "response", "failed", e
		failed, _ := json.Marshal(map[string]any{"type": "response.failed", "sequence_number": g.sequence + 2, "response": r})
		return []byte("event: error\ndata: " + string(b) + "\n\nevent: response.failed\ndata: " + string(failed) + "\n\n")
	}
	if g.protocol == chat || g.protocol == gemini {
		return []byte("data: " + string(b) + "\n\n")
	}
	return []byte("event: " + name + "\ndata: " + string(b) + "\n\n")
}
