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
	Kind  string          `json:"kind"` // "user" | "event" | "status" | "question" | "steer" | "delegation" | "chat_session" | "title" | "divider"(旧)
	Text  string          `json:"text,omitempty"`
	Event json.RawMessage `json:"event,omitempty"`
	// status: running | idle | error | interrupted (the user stopped the turn).
	// event: "interrupted" marks the CLI's failed result caused by that stop.
	State string        `json:"state,omitempty"`
	Error string        `json:"error,omitempty"`
	Turn  *TurnMetadata `json:"turn,omitempty"`
	// Local marks the idle status of a slash command turn the CLI ran itself
	// (/context, /compact…); it does not count toward the title.
	Local bool `json:"local,omitempty"`
	// DurationMS is the turn's wall time on its final status (idle, error,
	// interrupted), measured from admission like the live counter's
	// started_at, so the page can show how long the turn took.
	DurationMS int64 `json:"duration_ms,omitempty"`
}

// Runtime is the adapter to workspace, authorization, history and live clients.
// It grants no scheduling rights. HTTP receipt admission, the legacy room gate,
// or a durable delegation record owned by a running main turn must precede Run.
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
	// PromptSuggestions reports whether the started container's CLI can
	// predict the next prompt; an older image would reject the flag.
	PromptSuggestions(context.Context, store.Session) bool
	// Questions reports whether the started container's CLI can keep a turn
	// open for the user's answers.
	Questions(context.Context, store.Session) bool
	// Question offers q to the answer API until withdraw; only the first
	// answer reaches the channel, and none after withdraw returns.
	Question(agent.Question) (answers <-chan agent.Answer, withdraw func())
	Executor(store.Session) Executor
	Permission() string
	PublishCost(string, string)
	Title(string, string)
	// TurnEnded observes the end of every turn that got past admission, after
	// its usage is booked. It must not block.
	TurnEnded(TurnOutcome)
}

// Turn outcomes reported to Runtime.TurnEnded.
const (
	OutcomeCompleted   = "completed"
	OutcomeFailed      = "failed"
	OutcomeInterrupted = "interrupted"
)

// TurnOutcome summarizes a finished turn: identifiers, how it ended (with
// the problem code of a failure) and how long it took. No text.
type TurnOutcome struct {
	SessionID, ThreadID, TurnID string
	Model                       string
	Status, Code                string
	Duration                    time.Duration
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

// cliText is what the CLI reads for the turn: the user's text with the
// server-owned preface and context around it. The CLI only recognizes a slash
// command at the very start of the message, so a command goes without the
// preface rather than turning into a prompt about "/context".
func cliText(in Input, text string) string {
	out := text
	if in.Preface != "" && !agent.IsClaudeSlashCommand(text) {
		out = in.Preface + "\n\n" + out
	}
	if in.Context != "" {
		out += "\n\n" + in.Context
	}
	return out
}

type Input struct {
	SessionID, Text, Model, Effort, Control string
	Context                                 string
	StrictResume, RequireAppServer          bool
	// RequireUsage reports an unobserved usage result to the observer instead
	// of only logging it.
	RequireUsage bool
	// Timeout bounds the whole turn; zero keeps the ordinary 30 minutes.
	Timeout time.Duration
	// Extras are server-owned CLI additions (delegation tool, worker PID file).
	Extras agent.ChatExtras
	// Fresh ignores the session's provider conversation and uses Resume
	// instead: empty starts a new conversation, otherwise it continues that
	// one. A worker never continues the main agent's conversation.
	Resume string
	Fresh  bool
	// Meter adjusts the usage rows' attribution before they are recorded.
	Meter func(*store.UsageEvent)
	// Preface is server-owned text sent to the CLI before the user's text for
	// this turn only; history and titles keep the user's own text.
	Preface string
	// Suggestions asks for a predicted next prompt after the turn when the
	// container's CLI supports it; that extra request is charged separately.
	Suggestions bool
	// Questions lets the model ask the user (AskUserQuestion) and wait for
	// the answer within the turn. Only a page that can show the question
	// sets it; otherwise nobody could answer and the turn would just wait.
	Questions bool
	// Tasks turns on Claude Code's task list tools for Claude 5 models, which
	// the CLI leaves off; the user opted in to the extra tokens.
	Tasks bool
}
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

	timeout := in.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	// The limit pauses while a question waits for the user (see ask).
	ctx, cancelTurn := context.WithCancelCause(parent)
	clock := newTurnClock(timeout, func() { cancelTurn(context.DeadlineExceeded) })
	defer func() { clock.stop(); cancelTurn(context.Canceled) }()
	outcome := TurnOutcome{SessionID: in.SessionID, Status: OutcomeFailed}
	var began time.Time // set once the turn is admitted; see below
	record := func(e Entry, messageType, retryText string) error {
		if e.Kind == "status" && e.State != "running" && !began.IsZero() {
			e.DurationMS = max(time.Since(began).Milliseconds(), 1)
		}
		err := r.Record(e, messageType, retryText)
		if receipt != nil && err != nil {
			receipt.HistoryFailed()
		}
		return err
	}

	fail := func(err error, fallback string) {
		if errors.Is(err, context.Canceled) && errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			err = context.DeadlineExceeded // the turn ran out of time, nobody cancelled it
		}
		p := r.Problem(ctx, "chat.turn", err, fallback)
		outcome.Code = p.Code
		if receipt != nil {
			receipt.Fail(err, p.Code)
		}
		record(Entry{Kind: "status", State: "error", Error: p.Error, ProblemDetails: &p.ProblemDetails}, "status", "")
	}

	rejectOptions := func(_ string) {
		p := r.Problem(ctx, "chat.options", nil, "chat_options_invalid")
		outcome.Code = p.Code
		if receipt != nil {
			receipt.Fail(nil, p.Code)
		}
		record(Entry{Kind: "status", State: "error", Error: p.Error, ProblemDetails: &p.ProblemDetails}, "status", text)
	}
	if receipt != nil && !receipt.Advance(store.ChatStarting) {
		return
	}
	// Registered first so it runs last, after usage is flushed.
	began = time.Now()
	defer func() {
		outcome.Model, outcome.Duration = model, time.Since(began)
		r.TurnEnded(outcome)
	}()

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
	outcome.ThreadID = tid
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
	outcome.TurnID = turnID
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

	extras := in.Extras
	extras.PromptSuggestions = in.Suggestions && r.PromptSuggestions(ctx, sess)
	extras.Interactive = in.Questions && r.Questions(ctx, sess)
	extras.TaskTools = in.Tasks // Claude only; other agents ignore it

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
	if extras.PromptSuggestions {
		tally.MeterSuggestion()
	}

	// 容器起来之后才可能产生消耗，之后无论回合正常收尾、报错还是被中断，已经
	// 报上来的用量都要落库——钱花了就得记。一行都没记到说明这个 agent/版本报
	// 用量的形状我们没认出来：消耗真实发生了却不会进报表，宁可吵一句。
	// A slash command the CLI ran itself: /context has no usage to warn
	// about (/compact's summary request is billed from the CLI's cost-state),
	// and neither names the thread. Set by onLine from the CLI's own result.
	local := false
	defer func() {
		if s.Usage.Flush(&tally, time.Since(turnStart)) == 0 && !local {
			log.Printf("usage: 会话 %s 回合结束但未记到用量（agent=%s），该回合消耗不会进报表", in.SessionID, sess.Agent)
			if in.RequireUsage && receipt != nil {
				receipt.UsageFailed()
			}
		}
		r.PublishCost(tid, turnID)
		if receipt != nil && tally.SettlementError() != nil {
			receipt.UsageFailed()
		}
	}()

	// 两条路径共用的行处理：JSON 事件落盘并广播（增量只广播），
	// 顺带提取 provider 会话 id 供续聊；非 JSON 行按原文透传。
	chatID := sess.ChatSession
	if in.Fresh {
		chatID = in.Resume
	}
	// The user's stop ends the CLI's turn with a failed result; history shows
	// one interrupted status for it instead of that failure.
	executor := r.Executor(sess)
	userStopped := func() bool { return executor.Interrupted != nil && executor.Interrupted() }
	terminal := ""
	onLine := func(line []byte) {
		if len(line) == 0 {
			return
		}
		if event, valid := adapter.Decode(line); valid {
			if receipt != nil {
				receipt.Terminal(event.Terminal)
			}
			if event.Terminal != "" {
				terminal = event.Terminal
				local = event.Local
			}
			ev := event.Raw
			// 掐表要在增量分支之前：最早的模型输出往往就是个增量事件，放到
			// 后面量到的是第一个完整事件，那已经是整段话说完了。
			if ttftMS == 0 && event.Output {
				ttftMS = time.Since(turnStart).Milliseconds()
			}
			// Claude message_delta carries final output counts; meter it before
			// partial events take the broadcast-only return below.
			base := store.UsageEvent{
				User: sess.User, SessionID: sess.ID, ThreadID: tid, TurnID: turnID,
				Agent: sess.Agent, AccountID: sess.AccountID, Model: model,
				Kind: store.UsageKindChat, TTFTMs: ttftMS,
			}
			if in.Meter != nil {
				in.Meter(&base)
			}
			tally.Observe(base, line)
			if event.Partial {
				// 增量 delta 只广播不落盘：随后的完整事件会带全文再来一份
				r.Emit(map[string]any{"type": "agent_event", "event": ev, "ts": time.Now()})
				return
			}
			entry := Entry{Kind: "event", Event: ev}
			if event.Terminal != "" && event.Terminal != "completed" && userStopped() {
				entry.State = "interrupted"
			}
			record(entry, "agent_event", "")
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
	executor.OnLine = onLine
	if extras.Interactive {
		recordQuestion := func(v QuestionView) {
			raw, _ := json.Marshal(v)
			record(Entry{Kind: "question", Event: raw}, "question", "")
		}
		executor.Ask = func(qctx context.Context, q agent.Question) agent.Answer {
			return ask(qctx, q, r, clock, recordQuestion)
		}
		// A follow-up shows in history where the CLI took it, or as not applied.
		executor.Steered = func(id, text string, applied bool) {
			state := "applied"
			if !applied {
				state = "dropped"
			}
			raw, _ := json.Marshal(SteerView{ID: id, Text: text, State: state})
			record(Entry{Kind: "steer", Event: raw}, "steer", "")
		}
	}

	if err := executor.Run(ctx, adapter, Turn{SessionID: sess.ID, ContainerID: sess.ContainerID,
		Text: cliText(in, text), ChatID: chatID, Model: model, Effort: effort, Permission: r.Permission(), Options: options, StrictResume: in.StrictResume, RequireAppServer: in.RequireAppServer, Extras: extras}); err != nil {
		var exited *ExitError
		if (errors.Is(err, ErrInterrupted) || errors.As(err, &exited)) && userStopped() {
			if receipt != nil {
				receipt.Fail(err, "operation_cancelled")
			}
			outcome.Status = OutcomeInterrupted
			record(Entry{Kind: "status", State: "interrupted"}, "status", "")
			return
		}
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
	state := "idle"
	outcome.Status = OutcomeCompleted
	if terminal != "" && terminal != "completed" && userStopped() {
		state = "interrupted"
		outcome.Status = OutcomeInterrupted
	}
	record(Entry{Kind: "status", State: state, Local: local && state == "idle"}, "status", "")

	// 首条消息的对话：异步用模型总结出一个标题，不阻塞对话流。本地命令的回合
	// 没有可总结的内容，留给下一条真正的消息。
	if firstTurn && !local && tid != "" && (receipt == nil || receipt.Succeeded()) {
		r.Title(tid, text)
	}
}
