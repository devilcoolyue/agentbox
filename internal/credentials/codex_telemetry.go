package credentials

import (
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Codex has no env switch for OpenTelemetry: the export is an [otel] section
// in config.toml. The session copy names the server's receiver, whose URL
// authenticates that one session (like the gateway's), so it never goes into
// the account pool. Codex rewrites the file itself (the TUI expands the inline
// tables), which is why the section is recognised by its endpoint, not by its
// formatting.

// codexTelemetryConfig points a session config's [otel] section at endpoint,
// or removes the one we wrote when endpoint is "" (receiver off). An [otel]
// section exporting anywhere else is the operator's own collector and stays.
// Prompts are never logged (log_user_prompt = false, Codex's default spelled
// out); traces and metrics stay off.
func codexTelemetryConfig(raw []byte, endpoint, sessionID string) ([]byte, error) {
	settings := map[string]any{}
	if err := toml.Unmarshal(raw, &settings); err != nil {
		return nil, err
	}
	current, exists := settings["otel"]
	ours := false
	if section, ok := current.(map[string]any); ok {
		ours = codexTelemetryOurs(section, sessionID)
	}
	if exists && !ours {
		return raw, nil
	}
	if endpoint == "" {
		if !exists {
			return raw, nil
		}
		delete(settings, "otel")
		return toml.Marshal(settings)
	}
	settings["otel"] = map[string]any{
		"log_user_prompt": false,
		"exporter": map[string]any{"otlp-http": map[string]any{
			"endpoint": endpoint,
			"protocol": "json",
		}},
	}
	return toml.Marshal(settings)
}

// codexTelemetryOurs reports whether an [otel] section exports to this
// session's receiver path, wherever the receiver listened at the time.
func codexTelemetryOurs(section map[string]any, sessionID string) bool {
	exporter, _ := section["exporter"].(map[string]any)
	httpExporter, _ := exporter["otlp-http"].(map[string]any)
	endpoint, _ := httpExporter["endpoint"].(string)
	return strings.Contains(endpoint, "/otel/"+sessionID+"/") && strings.HasSuffix(endpoint, "/v1/logs")
}
