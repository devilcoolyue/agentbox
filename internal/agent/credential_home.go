package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"

	"agentbox/internal/safefs"
)

// ServerRefreshMarker in a Claude account pool means the server alone owns the
// refresh-token chain: session homes get the access token only, and nothing in
// a home is ever read back into the pool. Accounts without it keep the older
// two-way convergence until the server's first successful refresh writes it,
// so an upgrade never strands a chain that a CLI rotated moments before.
const ServerRefreshMarker = ".agentbox-server-refresh"

// ServerRefreshes reports whether the pool carries ServerRefreshMarker.
func ServerRefreshes(pool *safefs.Root) bool {
	info, err := pool.Lstat(ServerRefreshMarker)
	return err == nil && info.Mode().IsRegular()
}

// ClaudeHomeCredential returns the session copy of .credentials.json for a
// server-refreshed account and whether it differs from home.
//
// Only claudeAiOauth comes from the pool, minus the refresh token. Everything
// else is the session's own: Claude Code keeps MCP server OAuth tokens
// (mcpOAuth) in the same file, and those belong to whoever signed in from that
// workspace, never to the account. Comparing the decoded login rather than the
// bytes keeps the CLI's own rewrites of the file from causing churn.
func ClaudeHomeCredential(pool, home []byte) ([]byte, bool, error) {
	poolRoot, err := decodeCredential(pool)
	if err != nil {
		return nil, false, fmt.Errorf("account credential: %w", err)
	}
	login, ok := poolRoot["claudeAiOauth"].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("account credential has no claudeAiOauth")
	}
	scoped := make(map[string]any, len(login))
	for k, v := range login {
		if k != "refreshToken" {
			scoped[k] = v
		}
	}
	homeRoot := map[string]any{}
	if len(bytes.TrimSpace(home)) > 0 {
		// An unreadable session file is the CLI's to report; replacing it with
		// a working login is the useful repair.
		if decoded, err := decodeCredential(home); err == nil {
			homeRoot = decoded
		}
	}
	if reflect.DeepEqual(homeRoot["claudeAiOauth"], scoped) {
		return home, false, nil
	}
	homeRoot["claudeAiOauth"] = scoped
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(homeRoot); err != nil {
		return nil, false, err
	}
	return out.Bytes(), true, nil
}

func decodeCredential(raw []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // expiresAt must survive exactly as written
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, err
	}
	if root == nil {
		return nil, fmt.Errorf("credential is not a JSON object")
	}
	return root, nil
}
