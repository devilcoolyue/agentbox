package chat

import (
	"agentbox/internal/agent"
	"agentbox/internal/protocol"
	"agentbox/internal/store"
	"agentbox/internal/usage"
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"
)

type TurnMetadata struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Effort       string `json:"effort"`
	Control      string `json:"control"`
	Unsupported  bool   `json:"unsupported,omitempty"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type Entry struct {
	*protocol.ProblemDetails
	TS    time.Time       `json:"ts"`
	Kind  string          `json:"kind"` // "user" | "event" | "status" | "chat_session" | "title" | "divider"(旧)
	Text  string          `json:"text,omitempty"`
	Event json.RawMessage `json:"event,omitempty"`
	State string          `json:"state,omitempty"`
	Error string          `json:"error,omitempty"`
	Turn  *TurnMetadata   `json:"turn,omitempty"`
}

// Runtime is the adapter to workspace, authorization, history and live clients.
// It grants no scheduling rights. Only HTTP admission of a fresh receipt (or the
// existing legacy room gate) may call Run while holding room ownership.
type Runtime interface {
	Hold() func()
	Session() (store.Session, bool)
	Options(context.Context, store.Session, string, string, string, bool) (agent.TurnOptions, error)
	Thread(store.Session) (string, bool)
	Start(context.Context, store.Session) (store.Session, error)
	Attachments(store.Session, []string) error
	Record(Entry, string, string) error
	Emit(any)
	Problem(context.Context, string, error, string) protocol.APIProblem
	ProviderSession(store.Session, string) error
	Executor(store.Session) Executor
	Permission() string
	PublishCost(string, string)
	Title(string, string)
}
type Observation interface {
	Input() store.ChatRequest
	Advance(string) bool
	Fail(error, string)
	Terminal(string)
	Completed(context.Context)
	Finish()
	Interrupted() bool
	HistoryFailed()
	UsageFailed()
	Succeeded() bool
}
type Service struct {
	Runtime Runtime
	Usage   *usage.Service
}
type Input struct{ SessionID, Text, Model, Effort, Control string }
type Failure struct {
	Code  string
	Cause error
}

func (e *Failure) Error() string { return e.Code }
func (e *Failure) Unwrap() error { return e.Cause }

func (s Service) Run(parent context.Context, in Input, receipt Observation) {
	text, model, effort, control := in.Text, in.Model, in.Effort, in.Control
	r := s.Runtime
	if receipt != nil {
		defer receipt.Finish()
	}

	// An in-flight turn may run for many minutes (tests/builds); hold the
	// session so the idle reaper never stops the container mid-turn.
	release := r.Hold()
	defer release()

	ctx, cancel := context.WithTimeout(parent, 30*time.Minute)
	defer cancel()
	record := func(e Entry, messageType, retryText string) error {
		err := r.Record(e, messageType, retryText)
		if receipt != nil && err != nil {
			receipt.HistoryFailed()
		}
		return err
	}

	fail := func(err error, fallback string) {
		p := r.Problem(ctx, "chat.turn", err, fallback)
		if receipt != nil {
			receipt.Fail(err, p.Code)
		}
		record(Entry{Kind: "status", State: "error", Error: p.Error, ProblemDetails: &p.ProblemDetails}, "status", "")
	}

	rejectOptions := func(_ string) {
		p := r.Problem(ctx, "chat.options", nil, "chat_options_invalid")
		if receipt != nil {
			receipt.Fail(nil, p.Code)
		}
		record(Entry{Kind: "status", State: "error", Error: p.Error, ProblemDetails: &p.ProblemDetails}, "status", text)
	}
	if receipt != nil && !receipt.Advance(store.ChatStarting) {
		return
	}

	sess, ok := r.Session()
	if !ok {
		fail(nil, "session_not_found")
		return
	}

	adapter, err := agent.Lookup(sess.Agent)
	if err != nil {
		fail(err, "chat_failed")
		return
	}
	if model == "" {
		model = sess.DefaultModel
	}

	options, err := r.Options(ctx, sess, model, effort, control, false)
	if err != nil {
		if errors.Is(err, ErrOptions) {
			rejectOptions("")
		} else {
			fail(err, "chat_failed")
		}
		return
	}
	// Options may replace a workspace default the account no longer offers.
	if options.Model != "" {
		model = options.Model
	}

	// 记录发消息前该线程的状态：首条消息且尚无标题时，回合成功后据首条
	// 消息让模型生成一个简洁标题。房间占用期间线程不会被切换，tid 稳定，
	// 用量流水也挂在它上面。
	tid, firstTurn := r.Thread(sess)
	if receipt != nil && tid != receipt.Input().ThreadID {
		fail(nil, "chat_thread_changed")
		return
	}

	// 本回合的用量归属。turnID 把一个回合里按模型拆出的多行流水串起来；一个
	// 回合可能报好几次用量，tally 负责收敛成一份账（见 usage.go）。
	// onLine 由回合的读循环串行调用，普通变量即可。
	turnID := store.NewID()
	if receipt != nil {
		turnID = receipt.Input().TurnID
	}
	var tally usage.Tally
	// 回合计时的起点，首字延迟与墙钟总耗时共用一块表：容器就绪之后开表（别把
	// 拉容器的几秒算进去），第一个模型输出事件量首字（见 Adapter.Decode），回合
	// 收尾时量总耗时。两个数同源才可比——provider 自报的 duration_ms 不含 CLI
	// 自身启动的那两秒，单独拿它当总耗时会比首字还小。
	var turnStart time.Time
	var ttftMS int64

	r.Emit(map[string]any{"type": "status", "state": "running"})
	sess, err = r.Start(ctx, sess)
	if err != nil {
		code := "workspace_start_failed"
		var failure *Failure
		if errors.As(err, &failure) {
			code = failure.Code
		}
		fail(err, code)
		return
	}
	options, err = r.Options(ctx, sess, model, effort, control, true)
	if err != nil {
		if errors.Is(err, ErrOptions) {
			rejectOptions("")
		} else {
			fail(err, "chat_failed")
		}
		return
	}

	if receipt != nil {
		if err := r.Attachments(sess, receipt.Input().Request.Attachments); err != nil {
			fail(err, "chat_attachments_invalid")
			return
		}

		if receipt.Interrupted() || ctx.Err() != nil {
			fail(context.Canceled, "operation_cancelled")
			return
		}
	}
	if err := record(Entry{Kind: "user", Text: text, Turn: &TurnMetadata{
		ID: turnID, Model: model, Effort: effort, Control: options.Control,
		Unsupported: options.Unsupported, BudgetTokens: options.BudgetTokens,
	}}, "user_message", ""); receipt != nil && err != nil {
		fail(err, "internal_error")
		return
	}
	turnStart = time.Now()
	tally = s.Usage.NewChatTally(sess)

	// 容器起来之后才可能产生消耗，之后无论回合正常收尾、报错还是被中断，已经
	// 报上来的用量都要落库——钱花了就得记。一行都没记到说明这个 agent/版本报
	// 用量的形状我们没认出来：消耗真实发生了却不会进报表，宁可吵一句。
	defer func() {
		if s.Usage.Flush(&tally, time.Since(turnStart)) == 0 {
			log.Printf("usage: 会话 %s 回合结束但未记到用量（agent=%s），该回合消耗不会进报表", in.SessionID, sess.Agent)
		}
		r.PublishCost(tid, turnID)
		if receipt != nil && tally.SettlementError() != nil {
			receipt.UsageFailed()
		}
	}()

	// 两条路径共用的行处理：JSON 事件落盘并广播（增量只广播），
	// 顺带提取 provider 会话 id 供续聊；非 JSON 行按原文透传。
	chatID := sess.ChatSession
	onLine := func(line []byte) {
		if len(line) == 0 {
			return
		}
		if event, valid := adapter.Decode(line); valid {
			if receipt != nil {
				receipt.Terminal(event.Terminal)
			}
			ev := event.Raw
			// 掐表要在增量分支之前：最早的模型输出往往就是个增量事件，放到
			// 后面量到的是第一个完整事件，那已经是整段话说完了。
			if ttftMS == 0 && event.Output {
				ttftMS = time.Since(turnStart).Milliseconds()
			}
			// Claude message_delta carries final output counts; meter it before
			// partial events take the broadcast-only return below.
			tally.Observe(store.UsageEvent{
				User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: turnID,
				Agent: sess.Agent, AccountID: sess.AccountID, Model: model,
				Kind: store.UsageKindChat, TTFTMs: ttftMS,
			}, line)
			if event.Partial {
				// 增量 delta 只广播不落盘：随后的完整事件会带全文再来一份
				r.Emit(map[string]any{"type": "agent_event", "event": ev, "ts": time.Now()})
				return
			}
			record(Entry{Kind: "event", Event: ev}, "agent_event", "")
			if id := event.SessionID; id != "" && id != chatID {
				chatID = id
				if err := r.ProviderSession(sess, id); err != nil && receipt != nil {
					receipt.HistoryFailed()
				}

			}
		} else {
			r.Emit(map[string]any{"type": "agent_raw", "text": string(line)})
		}
	}

	if receipt != nil && !receipt.Advance(store.ChatRunning) {
		return
	}
	executor := r.Executor(sess)
	executor.OnLine = onLine

	if err := executor.Run(ctx, adapter, Turn{SessionID: sess.ID, ContainerID: sess.ContainerID,
		Text: text, ChatID: chatID, Model: model, Effort: effort, Permission: r.Permission(), Options: options}); err != nil {
		code := "chat_failed"
		if errors.Is(err, ErrOptions) {
			code = "chat_options_invalid"
		}
		fail(err, code)
		return
	}

	if receipt != nil {
		receipt.Completed(ctx)
	}
	record(Entry{Kind: "status", State: "idle"}, "status", "")

	// 首条消息的对话：异步用模型总结出一个标题，不阻塞对话流。
	if firstTurn && tid != "" && (receipt == nil || receipt.Succeeded()) {
		r.Title(tid, text)
	}
}
