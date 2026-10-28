// Package protocol defines the additive M1/M3 wire envelopes. It has no HTTP dependency.
package protocol

import "agentbox/internal/store"

const ChatVersion = 1

type ChatRequestResponse struct {
	Version  int               `json:"version"`
	Receipt  store.ChatRequest `json:"receipt"`
	Replayed *bool             `json:"replayed,omitempty"`
}
type ChatRequestList struct {
	Version  int                 `json:"version"`
	Scope    string              `json:"scope"`
	Requests []store.ChatRequest `json:"requests"`
	Pending  *store.ChatRequest  `json:"pending"`
	// Pendings lists every thread's active or unreviewed request: threads of
	// a workspace run side by side. Pending is the requested thread's, or the
	// oldest when no thread is named.
	Pendings []store.ChatRequest `json:"pendings,omitempty"`
}
type ChatRequestEvent struct {
	Type    string            `json:"type"`
	Version int               `json:"version"`
	Receipt store.ChatRequest `json:"receipt"`
}

func Response(receipt store.ChatRequest, replayed ...bool) ChatRequestResponse {
	result := ChatRequestResponse{Version: ChatVersion, Receipt: receipt}
	if len(replayed) > 0 {
		result.Replayed = &replayed[0]
	}
	return result
}
func Event(receipt store.ChatRequest) ChatRequestEvent {
	return ChatRequestEvent{Type: "chat_request", Version: ChatVersion, Receipt: receipt}
}
