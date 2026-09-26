package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"agentbox/internal/config"
)

// ProbeCodexModels only initializes and reads metadata; it never starts a
// thread or turn. The caller owns cancellation/closing of the transport.
func ProbeCodexModels(w io.Writer, r io.Reader) (map[string]config.ReasoningCapability, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 2<<20)
	c := &appServerConn{w: w, sc: sc, emit: func([]byte) {}}
	if _, err := c.call(1, "initialize", map[string]any{"clientInfo": map[string]string{"name": "agentbox-models", "version": "1.0"}}); err != nil {
		return nil, err
	}
	if err := c.send(map[string]any{"method": "initialized"}); err != nil {
		return nil, err
	}
	// Catalog entries bundled with Codex are not proof of a relay's support.
	raw, err := c.call(2, "config/read", map[string]any{"includeLayers": false, "cwd": "/workspace"})
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Config *struct {
			Provider  string `json:"model_provider"`
			Providers map[string]struct {
				BaseURL string `json:"base_url"`
			} `json:"model_providers"`
		} `json:"config"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, err
	}
	if cfg.Config == nil {
		return nil, fmt.Errorf("config/read missing config")
	}
	if cfg.Config.Providers["openai"].BaseURL != "" {
		return nil, nil
	}
	if cfg.Config.Provider != "" && cfg.Config.Provider != "openai" {
		return nil, nil
	}
	out := map[string]config.ReasoningCapability{}
	cursor := ""
	for page := 0; page < 10; page++ {
		params := map[string]any{"limit": 100}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(3+page, "model/list", params)
		if err != nil {
			return nil, err
		}
		var result struct {
			Data []struct {
				Model   string `json:"model"`
				Efforts []struct {
					Effort string `json:"reasoningEffort"`
				} `json:"supportedReasoningEfforts"`
			} `json:"data"`
			Next string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &result); err != nil {
			return nil, err
		}
		for _, m := range result.Data {
			if !modelRe.MatchString(m.Model) {
				continue
			}
			levels := []string{}
			for _, e := range m.Efforts {
				if slices.Contains(config.ReasoningLevels(config.AgentCodex, "effort"), e.Effort) && !slices.Contains(levels, e.Effort) {
					levels = append(levels, e.Effort)
				}
			}
			if len(levels) > 0 {
				out[m.Model] = config.ReasoningCapability{Support: "supported", Control: "effort", Levels: levels}
			}
		}
		if result.Next == "" {
			return out, nil
		}
		if result.Next == cursor {
			return nil, fmt.Errorf("model/list cursor did not advance")
		}
		cursor = result.Next
	}
	return nil, fmt.Errorf("model/list exceeded page limit")
}
