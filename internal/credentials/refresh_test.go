package credentials

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 刷新响应里没给的字段一律留原值，别把整份凭证重建掉。
func TestMergeClaudeCredKeepsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	before := `{"claudeAiOauth":{"accessToken":"old","refreshToken":"r0","expiresAt":1,` +
		`"subscriptionType":"max","rateLimitTier":"t20","scopes":["user:profile"],"custom":"keep-me"}}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := mergeClaude(path, OAuthTokenResponse{AccessToken: "new", ExpiresIn: 3600})
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new" || got.RefreshToken != "r0" || got.SubscriptionType != "max" {
		t.Errorf("返回值 = %+v，刷新令牌与订阅档位应沿用旧值", got)
	}
	if got.ExpiresAt <= time.Now().UnixMilli() {
		t.Errorf("expiresAt = %d，应是将来的时间", got.ExpiresAt)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		ClaudeAiOauth map[string]any `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	c := root.ClaudeAiOauth
	if c["refreshToken"] != "r0" || c["rateLimitTier"] != "t20" || c["custom"] != "keep-me" {
		t.Errorf("落盘的凭证丢字段了: %+v", c)
	}
	if scopes, _ := c["scopes"].([]any); len(scopes) != 1 || scopes[0] != "user:profile" {
		t.Errorf("scopes 被冲掉了: %+v", c["scopes"])
	}
	// 毫秒时间戳不能被写成科学计数法，CLI 那边是按整数读的。
	if want := fmt.Sprintf(`"expiresAt": %d`, got.ExpiresAt); !strings.Contains(string(raw), want) {
		t.Errorf("expiresAt 落盘形状不对，want %s: %s", want, raw)
	}
}
