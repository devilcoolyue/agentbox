package mcpconfig

import _ "embed"

// Helper is executed as python3 -I -c inside the session container, so existing
// agent images work without installing files or trusting writable home scripts.
//
//go:embed helper.py
var Helper string

type CheckResult struct {
	Status    string `json:"status"`
	Tools     []Tool `json:"tools,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	CheckedAt string `json:"checked_at,omitempty"`
}
type Tool struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}
