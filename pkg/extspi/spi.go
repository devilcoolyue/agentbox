// Package extspi defines the Agentbox extension SPI: the messages exchanged
// between the extension host and an extension, the Handler an extension
// implements, and an NDJSON JSON-RPC 2.0 transport over stdio.
//
// The same Handler runs in-process for builtin extensions and behind Serve for
// out-of-process ones; Pipe connects the two for conformance tests. The design
// is documented in docs/architecture/extensions-spi.md.
package extspi

import (
	"context"
	"encoding/json"
	"fmt"
)

// Version is the SPI version this package speaks.
const Version = "1.0"

// MaxMessageBytes bounds one NDJSON message in either direction.
const MaxMessageBytes = 1 << 20

// Methods.
const (
	MethodInitialize   = "initialize"
	MethodInitialized  = "initialized"
	MethodShutdown     = "shutdown"
	MethodExit         = "exit"
	MethodPing         = "ping"
	MethodModelRequest = "model/request"
	MethodStreamOpen   = "model/stream/open"
	MethodStreamDelta  = "model/stream/delta"
	MethodStreamClose  = "model/stream/close"
	MethodStreamAbort  = "model/stream/abort"
	MethodStatusReport = "status/report"
	MethodLog          = "log"
)

// Hooks an extension may subscribe to.
const (
	HookModelRequest = "model.request"
	HookModelStream  = "model.stream"
)

// Permissions an administrator may grant.
const (
	PermStreamText         = "model.stream.text"
	PermRequestFingerprint = "model.request.fingerprint"
	PermContext            = "model.context"
)

// HookPermission is the permission a hook cannot run without.
var HookPermission = map[string]string{
	HookModelRequest: PermRequestFingerprint,
	HookModelStream:  PermStreamText,
}

// Request verdict actions.
const (
	ActionAllow  = "allow"
	ActionReject = "reject"
)

// Stream close reasons.
const (
	CloseCompleted     = "completed"
	CloseUpstreamError = "upstream_error"
	CloseClientClosed  = "client_closed"
	CloseAborted       = "aborted"
	CloseRejected      = "rejected"
	CloseNotInspected  = "not_inspected"
	CloseOverflow      = "overflow"
	CloseTimeout       = "timeout"
)

// SPI error codes, alongside the JSON-RPC 2.0 standard ones.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodePermissionDenied = -32010
	CodeUnknownStream    = -32011
	CodeInvalidVerdict   = -32012
	CodeRateLimited      = -32013
)

// Error is a JSON-RPC error. Handlers may return it to choose the code.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("extspi error %d: %s", e.Code, e.Message) }

type HostInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type ExtensionInfo struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

type Limits struct {
	RequestTimeoutMS int `json:"request_timeout_ms"`
	MaxMessageBytes  int `json:"max_message_bytes"`
}

type InitializeParams struct {
	SPIVersions []string        `json:"spi_versions"`
	Host        HostInfo        `json:"host"`
	Extension   ExtensionInfo   `json:"extension"`
	Config      json.RawMessage `json:"config"`
	Permissions []string        `json:"permissions"`
	Limits      Limits          `json:"limits"`
}

type InitializeResult struct {
	SPIVersion string   `json:"spi_version"`
	Hooks      []string `json:"hooks"`
}

// Context identifies who a stream belongs to. Only sent with model.context.
type Context struct {
	Session string `json:"session,omitempty"`
	Account string `json:"account,omitempty"`
	User    string `json:"user,omitempty"`
	Source  string `json:"source,omitempty"` // chat | terminal | unknown
	Turn    string `json:"turn,omitempty"`
}

type ModelRequestParams struct {
	Stream      string   `json:"stream"`
	Protocol    string   `json:"protocol"`
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Agent       string   `json:"agent,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	Context     *Context `json:"context,omitempty"`
}

type RequestVerdict struct {
	Action  string `json:"action"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type StreamOpenParams struct {
	Stream   string   `json:"stream"`
	Protocol string   `json:"protocol"`
	Agent    string   `json:"agent,omitempty"`
	Context  *Context `json:"context,omitempty"`
}

// Delta is one decoded text increment; see modelgw.Delta.
type Delta struct {
	Part    string `json:"part"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
}

type StreamDeltaParams struct {
	Stream     string   `json:"stream"`
	Seq        int64    `json:"seq"`
	Deltas     []Delta  `json:"deltas"`
	EndedParts []string `json:"ended_parts,omitempty"`
}

type StreamCloseParams struct {
	Stream    string `json:"stream"`
	Reason    string `json:"reason"`
	AbortedBy string `json:"aborted_by,omitempty"`
	Code      string `json:"code,omitempty"`
}

type AbortParams struct {
	Stream  string             `json:"stream"`
	Code    string             `json:"code"`
	Message string             `json:"message"`
	Detail  map[string]float64 `json:"detail,omitempty"`
}

type AbortResult struct {
	Applied bool   `json:"applied"`
	Reason  string `json:"reason,omitempty"` // closed | superseded
}

type StatusReport struct {
	State   string             `json:"state"` // ok | degraded
	Metrics map[string]float64 `json:"metrics,omitempty"`
}

type LogParams struct {
	Level   string `json:"level"` // info | warn | error
	Message string `json:"message"`
}

// Handler is implemented by an extension. Stream notifications for one stream
// arrive in order; handlers must return promptly and must not block on model
// traffic. Embed BaseHandler to inherit no-op defaults.
type Handler interface {
	Initialize(ctx context.Context, p InitializeParams, host Host) (InitializeResult, error)
	ModelRequest(ctx context.Context, p ModelRequestParams) (RequestVerdict, error)
	StreamOpen(p StreamOpenParams)
	StreamDelta(p StreamDeltaParams)
	StreamClose(p StreamCloseParams)
	Shutdown(ctx context.Context) error
}

// Host is what an extension may call. It is safe for concurrent use.
type Host interface {
	Abort(ctx context.Context, p AbortParams) (AbortResult, error)
	ReportStatus(p StatusReport)
	Log(p LogParams)
}

// BaseHandler allows every request and ignores every notification.
type BaseHandler struct{}

func (BaseHandler) ModelRequest(context.Context, ModelRequestParams) (RequestVerdict, error) {
	return RequestVerdict{Action: ActionAllow}, nil
}
func (BaseHandler) StreamOpen(StreamOpenParams)    {}
func (BaseHandler) StreamDelta(StreamDeltaParams)  {}
func (BaseHandler) StreamClose(StreamCloseParams)  {}
func (BaseHandler) Shutdown(context.Context) error { return nil }
