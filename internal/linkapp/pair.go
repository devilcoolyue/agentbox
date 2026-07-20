package linkapp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agentbox/internal/tunnel"
)

// Pair turns a pasted pairing code into stored credentials: it decodes the
// server address, redeems the single-use secret there for a session token, and
// merges the result into cfg (leaving the user's allow rules and maps alone).
func Pair(cfg Config, code string, insecure bool) (Config, error) {
	payload, err := tunnel.DecodePairCode(code)
	if err != nil {
		return cfg, err
	}
	base, err := baseURL(payload.Server)
	if err != nil {
		return cfg, err
	}

	body, _ := json.Marshal(map[string]string{"code": payload.Code})
	resp, err := httpClient(insecure).Post(base.String()+"/api/tunnel/pair/redeem", "application/json", bytes.NewReader(body))
	if err != nil {
		return cfg, fmt.Errorf("连接 %s 失败：%w", base.Host, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		var out struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &out) == nil && out.Error != "" {
			return cfg, fmt.Errorf("配对失败：%s", out.Error)
		}
		return cfg, fmt.Errorf("配对失败：%s", strings.TrimSpace(resp.Status))
	}
	var out struct {
		User  string `json:"user"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.User == "" || out.Token == "" {
		return cfg, fmt.Errorf("服务器返回的配对结果不完整")
	}

	cfg.Server = base.String()
	cfg.User = out.User
	cfg.Token = out.Token
	cfg.Insecure = insecure
	return cfg, nil
}
