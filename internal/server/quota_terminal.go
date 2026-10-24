package server

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"

	"agentbox/internal/store"
)

// 终端里的 Agent 在余额见底时当场停下。终端用量是补记扣款的（store.UpsertTerminalUsage），
// 断开网页终端拦不住它：tmux detach 不杀进程，挂着自动迭代的 CLI 会一直跑下去。
// 所以每次用量入账后，用户若已被额度拦住，就给他每个运行中空间里由终端启动的
// claude / codex 发 SIGTERM。实测（Claude Code 2.1.309、Codex 0.164）两者都正常
// 退出：Claude 留下会话记录并打印 `claude --resume <id>`，Codex 的 node 外壳把信号
// 转给原生进程后一起退出。充值后在终端里 `claude -c` 就能接着来。
//
// 和网页对话「见底的那个回合允许超支」不同：终端的一个回合可以连跑几个小时，
// 等它自己结束等于不拦（管理员已定：立刻打断）。

// quotaStopInterval 是同一空间两次停止之间的最短间隔。停下后还会有几行用量陆续
// 入账（最后一次请求、补扫），不必每行都 docker exec 一次。
const quotaStopInterval = 15 * time.Second

type quotaTerminalStops struct {
	mu   sync.Mutex
	last map[string]time.Time // session id → last stop
	// run replaces the docker exec in tests.
	run func(ctx context.Context, containerID, script string) (string, error)
}

// noteUsageForQuota runs after every committed usage write.
func (s *Server) noteUsageForQuota(writes []store.UsageWrite) {
	users := map[string]bool{}
	for _, w := range writes {
		if users[w.User] {
			continue
		}
		users[w.User] = true
		if why := s.quotaBlock(w.User); why != "" {
			s.stopTerminalAgents(w.User, why)
		}
	}
}

func (s *Server) stopTerminalAgents(user, why string) {
	for _, sess := range s.store.List(user) {
		if sess.Status != store.StatusRunning || sess.ContainerID == "" || !s.quotaStops.claim(sess.ID) {
			continue
		}
		sess := sess
		s.spawn(func() {
			ctx, cancel := context.WithTimeout(s.workContext(), 20*time.Second)
			defer cancel()
			out, err := s.quotaStops.exec(s, ctx, sess.ContainerID, terminalAgentStopScript(why))
			if err != nil {
				log.Printf("quota: stop terminal agents in %s: %v", sess.ID, err)
				return
			}
			if n := strings.Count(out, "\n"); n > 0 {
				log.Printf("quota: %s 额度已用完，停止了空间 %s 终端里的 %d 个 Agent 进程", user, sess.ID, n)
			}
		})
	}
}

func (q *quotaTerminalStops) claim(session string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	now := time.Now()
	if t, ok := q.last[session]; ok && now.Sub(t) < quotaStopInterval {
		return false
	}
	if q.last == nil {
		q.last = map[string]time.Time{}
	}
	for id, t := range q.last {
		if now.Sub(t) >= quotaStopInterval {
			delete(q.last, id)
		}
	}
	q.last[session] = now
	return true
}

func (q *quotaTerminalStops) exec(s *Server, ctx context.Context, containerID, script string) (string, error) {
	if q.run != nil {
		return q.run(ctx, containerID, script)
	}
	return s.dock.ExecCommand(ctx, containerID, []string{"timeout", "-k", "2", "15", "/bin/sh", "-c", script})
}

// terminalAgentStopScript 停掉容器里由终端启动的 claude / codex，每停一个打印一行 pid。
//
// 「由终端启动」按进程祖先判断：网页对话、起标题、缓存保活都是服务端 docker exec
// 直接拉起的，往上找到的第一个父进程号为 0 的就是它们自己（或包着它们的 shell）；
// 终端里的程序跑在 tmux 服务端下面，tmux 服务端脱离终端后父进程是 1（docker-init），
// 用户 nohup / setsid 出去的同样落到 1。只停后者，网页回合照「见底的回合允许超支」跑完。
// Codex 只认原生进程：node 外壳 comm 是 node，信号由它转发、随子进程退出。
//
// 停下后把原因写进该进程的终端：先退出备用屏、恢复光标，免得 TUI 被杀后提示看不见。
func terminalAgentStopScript(why string) string {
	msg := shellQuote("\033[?1049l\033[?25h\r\n[agentbox] 已停止终端里的 Agent：" + why + "\r\n")
	return `msg=` + msg + `
for d in /proc/[0-9]*; do
	pid=${d#/proc/}
	comm=$(cat "$d/comm" 2>/dev/null) || continue
	case "$comm" in claude|claude.exe|codex) ;; *) continue ;; esac
	q=$pid; top=
	while [ -n "$q" ]; do
		pp=$(sed -n 's/^PPid:[[:space:]]*//p' "/proc/$q/status" 2>/dev/null)
		case "$pp" in
		0) top=exec; break ;;
		1) top=init; break ;;
		'') break ;;
		esac
		q=$pp
	done
	[ "$top" = init ] || continue
	tty=$(readlink "$d/fd/0" 2>/dev/null)
	kill -TERM "$pid" 2>/dev/null || continue
	echo "$pid"
	case "$tty" in /dev/pts/*) (sleep 1; printf '%s' "$msg" >"$tty") 2>/dev/null & ;; esac
done
wait
`
}
