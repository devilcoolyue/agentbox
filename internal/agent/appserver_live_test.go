package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestRunCodexTurnLive 对着真实的 codex app-server 进程跑一整回合，验证
// 驱动器与线上协议的兼容性。会消耗少量模型额度且依赖本机 codex 登录态，
// 故默认跳过：CODEX_LIVE_TEST=1 go test -run TestRunCodexTurnLive ./internal/agent/
func TestRunCodexTurnLive(t *testing.T) {
	if os.Getenv("CODEX_LIVE_TEST") == "" {
		t.Skip("需要 CODEX_LIVE_TEST=1（真实调用 codex，消耗额度）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, "codex", "app-server")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("codex 不可用: %v", err)
	}
	defer cmd.Wait()
	defer stdin.Close()

	var events []string
	err = RunCodexTurn(ctx, stdin, stdout,
		func() { stdin.Close() }, nil,
		CodexTurn{Prompt: "数到3，只输出数字。", Effort: "low", Cwd: t.TempDir()},
		func(line []byte) { events = append(events, string(line)) })
	if err != nil {
		t.Fatalf("回合失败: %v\n%s", err, strings.Join(events, "\n"))
	}
	joined := strings.Join(events, "\n")
	for _, frag := range []string{
		`"type":"thread.started"`,
		`"type":"content_block_delta"`, // 真流式增量确实在流
		`"type":"agent_message"`,
		`"type":"turn.completed"`,
	} {
		if !strings.Contains(joined, frag) {
			t.Errorf("事件流缺少 %s\n%s", frag, joined)
		}
	}
}
