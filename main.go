// reasonix-telegram: Telegram bridge for Reasonix (DeepSeek-Reasonix coding agent).
// - Keeps one long-lived `reasonix serve` per Telegram chat (multi-turn session)
// - Persists session files under /var/lib/reasonix-telegram; restores on bridge restart
// - Streams replies via Telegram draft or edit-in-place (no status-line prefix)
// - Chat-only mode: dedicated workdir + reasonix.toml disables all tools
// - /new starts a fresh Reasonix session
// - Configurable max output length, allow-listed users (optional)
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// Telegram shows "typing…" for ~5s per sendChatAction; refresh before it expires.
const typingRefreshInterval = 4 * time.Second

// Strip ANSI / TUI art from reasonix output. Reasonix in `run` mode emits
// progress blocks like "\x1b[2m  ▎ thinking [0m" which render as garbage
// in Telegram. We strip CSI sequences (\x1b[...m) and OSC sequences, plus
// the bracket-art lines reasonix draws between blocks.
//
// We keep the textual content; if the user actually wants TUI fidelity,
// they should run reasonix locally, not through a chat bot.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
var ansiOSC = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
var ansiBare = regexp.MustCompile(`\x1b[@-_]`) // ESC followed by a single byte (e.g. ESC c, ESC =)

func stripANSI(s string) string {
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiCSI.ReplaceAllString(s, "")
	s = ansiBare.ReplaceAllString(s, "")
	return s
}

// Lines matching these patterns are reasonix's own progress / status metadata,
// not part of the agent's actual answer. Filter them out so the user sees only
// the meaningful content in TG.
var (
	// "  · 7646 tok · in 7580 (7552 cached / 28 new) · out 66 (23 reasoning) · ¥0.0003"
	reTokenStats = regexp.MustCompile(`^\s*·\s*\d+\s*tok\b`)
	// "  · codegraph: fetching code-intelligence runtime in the background (one-time) — symbol-graph tools available next session"
	// Catch any `· <text>:` status line that reasonix prints
	reStatusDot = regexp.MustCompile(`^\s*·\s+\S+:`)
	// "  ▎ thinking" / "  ▎ ..."   (TUI bullet bar)
	reThinkingBar = regexp.MustCompile(`^\s*[▎▌▍▏┃│]\s*(thinking|reasoning|done|working|executing|reading|writing|searching)\b`)
	// Any line that is just whitespace after stripping
)

// isReasonixNoise returns true if the line should be dropped before display.
func isReasonixNoise(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "❌") || strings.HasPrefix(trimmed, "✅") || strings.HasPrefix(trimmed, "ℹ️") || strings.HasPrefix(trimmed, "hook ") || strings.Contains(trimmed, "exit status") || strings.Contains(trimmed, "remembered") {
		return true
	}
	return reTokenStats.MatchString(line) ||
		reStatusDot.MatchString(line) ||
		reThinkingBar.MatchString(line)
}

// Config: env-driven. Copy `.env.example` and fill in.
const (
	ModeChat = "chat"
	ModeTool = "tool"
)

// Callback data prefixes and actions.
const (
	prefixApproval = "ap:"
	prefixClarify  = "cl:"
	prefixModel    = "md:"
	prefixEffort   = "ef:"
	actionOnce     = "once"
	actionSession  = "session"
	actionDeny     = "deny"
	actionOther    = "other"
	actionPage     = "page"
)

// Available models — populated from reasonix doctor --json at startup.
var availableModels = []struct {
	ID   string
	Name string
}{}

func loadModelsFromReasonix(bin string) {
	cmd := exec.Command(bin, "doctor", "--json")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("loadModels: reasonix doctor failed: %v", err)
		return
	}
	var doc struct {
		Config   struct {
			DefaultModel string `json:"default_model"`
		} `json:"config"`
		Providers []struct {
			Name        string   `json:"name"`
			Models      []string `json:"models"`
			IsDefault   bool     `json:"is_default"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		log.Printf("loadModels: parse doctor json: %v", err)
		return
	}
	// Default provider name.
	defProv := doc.Config.DefaultModel
	availableModels = availableModels[:0]
	for _, p := range doc.Providers {
		isDefProv := p.Name == defProv
		for _, m := range p.Models {
			id := p.Name + "/" + m
			display := p.Name + ": " + m
			if isDefProv {
				display += " ⭐"
			}
			availableModels = append(availableModels, struct {
				ID   string
				Name string
			}{id, display})
		}
	}
	log.Printf("loadModels: loaded %d models from reasonix: %v", len(availableModels), func() []string {
		ids := make([]string, len(availableModels))
		for i, m := range availableModels {
			ids[i] = m.ID
		}
		return ids
	}())
}

func modelByID(id string) (string, bool) {
	for _, m := range availableModels {
		if m.ID == id {
			return m.Name, true
		}
	}
	return "", false
}

// Config: env-driven. Copy `.env.example` and fill in.
type Config struct {
	BotToken       string  // TG_BOT_TOKEN
	ReasonixBin  string  // REASONIX_BIN, default "reasonix"
	AllowedUsers []int64 // ALLOWED_USERS, comma-separated TG user IDs; empty = anyone
	MaxOutputBytes int     // MAX_OUTPUT_BYTES, default 524288 (stream buffer before split-send)
	MaxDuration    int     // MAX_DURATION_MIN, default 30
	Model          string  // MODEL, default "" (reasonix default)
	StateDir string // STATE_DIR, default /var/lib/reasonix-telegram
	Mode     string // MODE: "chat" (default, tools locked) or "tool" (full agent access)
}

func loadConfig() Config {
	mode := getenv("MODE", ModeChat)
	if mode != ModeChat && mode != ModeTool {
		mode = ModeChat
	}
	c := Config{
		BotToken:       os.Getenv("TG_BOT_TOKEN"),
		ReasonixBin:    getenv("REASONIX_BIN", "reasonix"),
		MaxOutputBytes: atoi(getenv("MAX_OUTPUT_BYTES", "524288")),
		MaxDuration:    atoi(getenv("MAX_DURATION_MIN", "30")),
		Model:          os.Getenv("MODEL"),
		StateDir: getenv("STATE_DIR", defaultStateDir),
		Mode:           mode,
	}
	if s := os.Getenv("ALLOWED_USERS"); s != "" {
		for _, p := range strings.Split(s, ",") {
			if id, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64); err == nil {
				c.AllowedUsers = append(c.AllowedUsers, id)
			} else {
				log.Printf("config: ALLOWED_USERS ignoring invalid id %q: %v", p, err)
			}
		}
	}
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("config: numeric value %q invalid, using 0", s)
	}
	return n
}

type clarifyState struct {
	question   string
	choices    []string
	askID      string
	questionID string
	port       int // reasonix serve port for submitting answer
	clarifyID     string
	awaitingCustom bool   // true after user clicks "Other", cleared on text input
	messageID     int    // Telegram message ID with the inline keyboard
	// Multi-question tracking
	allQuestions []askQuestionData // all questions in the ask
	qIndex       int               // which question we're on (0-based)
	answers      map[string][]string // questionID -> selected answers
}

type approvalState struct {
	approvalID string
	toolName   string
	port       int
}

// session: one per Telegram chat — workdir, Reasonix session file, serve process.
type session struct {
	mu               sync.Mutex
	workdir          string
	sessionPath      string
	servePort        int
	serveCmd         *exec.Cmd
	task             *runningTask // non-nil while a turn is in flight
	lastActivity     time.Time    // last message activity, for /sessions
	pendingClarify   *clarifyState  // non-nil while awaiting user clarify answer
	pendingApproval  *approvalState // non-nil while awaiting user approval
	wakePusher       func() // signal the turn pusher to check for new content
	model            string // per-session model override (empty = use global)
	lastUsage        wireUsage // latest usage data from serve
	// Cumulative session totals (accumulated across turns).
	cumPrompt     int
	cumCompletion int
	cumTotal      int
	cumCost       float64
	cumCurrency   string
	liveDraftID   int64 // open sendMessageDraft on Telegram (session-level for pre-empt cleanup)
}

type runningTask struct {
	cancel context.CancelFunc
}

type App struct {
	cfg        Config
	bot        *tgbotapi.BotAPI
	state      *stateStore
	sessMu     sync.Mutex
	sess       map[int64]*session
	restartMu      sync.Mutex
	restarting     bool
	restartStarted time.Time
	msgWg          sync.WaitGroup // tracks in-flight handleMessage goroutines
	draftSeq       uint64 // per-process draft_id sequence (avoids same-second collisions)
	clarifySeq     uint64 // monotonic counter for clarify IDs
	mode           atomic.Value // string: ModeChat or ModeTool
}

type tokenRedactingTransport struct {
	inner http.RoundTripper
	token string
}

func (t *tokenRedactingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil && t.token != "" {
		sanitized := strings.ReplaceAll(err.Error(), t.token, "***")
		if sanitized != err.Error() {
			return resp, errors.New(sanitized)
		}
	}
	return resp, err
}

func (a *App) getMode() string {
	if v := a.mode.Load(); v != nil {
		return v.(string)
	}
	return ModeChat
}

func (a *App) setMode(m string) {
	a.mode.Store(m)
}

func modeLabelFor(mode string) string {
	if mode == ModeTool {
		return "⌨️ 编程模式"
	}
	return "💬 聊天模式"
}

func (a *App) modeLabel() string {
	return modeLabelFor(a.getMode())
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := loadConfig()
	if cfg.BotToken == "" {
		log.Fatal("TG_BOT_TOKEN is required")
	}
	if _, err := exec.LookPath(cfg.ReasonixBin); err != nil {
		log.Fatalf("reasonix binary not found on PATH: %s (%v)", cfg.ReasonixBin, err)
	}
	loadModelsFromReasonix(cfg.ReasonixBin)

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("telegram auth failed: %v", err)
	}
	bot.Debug = false
	// Wrap HTTP client to redact bot token from error logs
	bot.Client = &http.Client{
		Transport: &tokenRedactingTransport{
			inner: http.DefaultTransport,
			token: cfg.BotToken,
		},
	}
	log.Printf("authorized as @%s (id=%d)", bot.Self.UserName, bot.Self.ID)

	if err := registerCommands(bot); err != nil {
		log.Printf("warning: setMyCommands failed: %v", err)
	} else {
		log.Printf("registered bot commands with Telegram")
	}

	st, err := newStateStore(cfg.StateDir)
	if err != nil {
		log.Fatalf("state dir: %v", err)
	}
	app := &App{cfg: cfg, bot: bot, state: st, sess: map[int64]*session{}}
	app.setMode(cfg.Mode)
	if err := app.ensureChatWorkdir(); err != nil {
		log.Fatalf("chat workdir: %v", err)
	}
	log.Printf("mode=%s workdir=%s", app.cfg.Mode, app.chatWorkdir())
	log.Printf("telegram stream: sendMessageDraft + sendMessage finalize (TelePi/Hermes pattern)")

	app.startRestartWatchdog()
	app.cleanupStaleServesOnStartup()
	app.restorePersistedSessions()
	app.notifyBridgeRestarted()

	// Shutdown handling: signal → drain handlers → cancel tasks → stop serves.
	// restarting is set first so the update loop below stops accepting new work;
	// then msgWg is drained; remaining tasks cancelled; serves stopped.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	shutdownCh := make(chan struct{})
	go func() {
		s := <-sig
		log.Printf("shutdown signal: received %v, draining message handlers…", s)
		app.restartMu.Lock()
		app.restarting = true
		app.restartMu.Unlock()

		drainDone := make(chan struct{})
		go func() {
			app.msgWg.Wait()
			close(drainDone)
		}()
		select {
		case <-drainDone:
			log.Printf("shutdown: all message handlers drained")
		case <-time.After(15 * time.Second):
			log.Printf("shutdown: drain timeout, proceeding with cancel")
		}
		app.cancelAllTasks()
		app.waitTasksDone(5 * time.Minute)
		log.Printf("shutdown: all tasks done, stopping serves…")
		app.stopAllServes()
		close(shutdownCh)
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for upd := range updates {
		if app.restarting {
			break
		}
		if upd.Message != nil {
			app.msgWg.Add(1)
			go func(msg *tgbotapi.Message) {
				defer app.msgWg.Done()
				app.handleMessage(msg)
			}(upd.Message)
		}
		if upd.CallbackQuery != nil {
			app.msgWg.Add(1)
			go func(query *tgbotapi.CallbackQuery) {
				defer app.msgWg.Done()
				app.handleCallbackQuery(query)
			}(upd.CallbackQuery)
		}
	}
	// Wait for shutdown to finish (serve processes stopped, etc).
	<-shutdownCh
	log.Print("shutdown complete")
}

// registerCommands tells Telegram which slash commands to surface in the
// client's `/` menu and the menu button. Without this, the client only
// shows commands the local user has previously used.
func registerCommands(bot *tgbotapi.BotAPI) error {
	cmds := []tgbotapi.BotCommand{
		{Command: "start", Description: "欢迎与指令说明"},
		{Command: "help", Description: "指令说明"},
		{Command: "status", Description: "是否在生成回复"},
		{Command: "stop", Description: "中止当前回复"},
		{Command: "new", Description: "开启新对话"},
		{Command: "restart", Description: "重启桥接"},
		{Command: "health", Description: "所有 serve 进程状态"},
		{Command: "sessions", Description: "活跃会话列表"},
		{Command: "chat", Description: "切换到聊天模式"},
		{Command: "code", Description: "切换到编程模式"},
		{Command: "model", Description: "切换模型：/model [名称]"},
	}
	_, err := bot.Request(tgbotapi.NewSetMyCommands(cmds...))
	return err
}

func (a *App) handleMessage(m *tgbotapi.Message) {
	if m.From == nil {
		return
	}
	if !a.allowed(m.From) {
		a.reply(m.Chat.ID, "⛔ 无权使用此机器人")
		return
	}
	a.restartMu.Lock()
	restarting := a.restarting
	a.restartMu.Unlock()
	if restarting {
		a.reply(m.Chat.ID, "🔄 桥接重启中，完成后会自动通知。")
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}

	// Check if we're awaiting a clarify answer.
	s := a.getOrCreateSession(m.Chat.ID)
	s.mu.Lock()
	pc := s.pendingClarify
	log.Printf("chat=%d: handleMessage text=%q pendingClarify=%v", m.Chat.ID, logPreview(text, 40), pc != nil)
	if pc != nil {
		// Store text answer (under lock to avoid concurrent-map write).
		answerText := text
		if pc.awaitingCustom {
			pc.awaitingCustom = false
			answerText = "(自定义) " + text
		}
		log.Printf("chat=%d: clarify text answer for q=%s: %q", m.Chat.ID, pc.questionID, answerText)
		pc.answers[pc.questionID] = []string{answerText}

		nextIdx := pc.qIndex + 1
		if nextIdx < len(pc.allQuestions) {
			// Advance to next question (all fields mutated under lock).
			nextQ := pc.allQuestions[nextIdx]
			pc.qIndex = nextIdx
			pc.questionID = nextQ.ID
			pc.choices = nextQ.Options
			cidNum := atomic.AddUint64(&a.clarifySeq, 1)
			pc.clarifyID = strconv.FormatUint(cidNum, 36)
			// Snapshot data needed outside lock.
			qText := _escapeMdv2(strings.TrimSpace(nextQ.Text))
			if qText == "" {
				qText = _escapeMdv2(strings.TrimSpace(nextQ.ID))
			}
			if qText == "" {
				qText = "请选择："
			}
			header := fmt.Sprintf("问题 %d/%d\n", nextIdx+1, len(pc.allQuestions))
			options := nextQ.Options
			clarifyID := pc.clarifyID
			prevMsgID := pc.messageID
			s.mu.Unlock()

			a.removeKeyboard(m.Chat.ID, prevMsgID)
			replyText := "❓ " + header + qText
			msg := newMessage(m.Chat.ID, replyText)
			msg.ParseMode = "MarkdownV2"
			if len(options) > 0 {
				var rows [][]tgbotapi.InlineKeyboardButton
				for i, choice := range options {
					data := fmt.Sprintf("%s%s:%d", prefixClarify, clarifyID, i)
					btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
					rows = append(rows, []tgbotapi.InlineKeyboardButton{
						{Text: btnText, CallbackData: &data},
					})
				}
				otherData := fmt.Sprintf("%s%s:%s", prefixClarify, clarifyID, actionOther)
				rows = append(rows, []tgbotapi.InlineKeyboardButton{
					{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
				})
				msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
			}
			if sent, err := a.sendWithRetry(msg, m.Chat.ID); err != nil {
				log.Printf("send failed: %v", err)
			} else {
				s.mu.Lock()
				s.pendingClarify.messageID = sent.MessageID
				s.mu.Unlock()
			}
			return
		}

		// All answered — submit.
		prevMsgID := pc.messageID
		s.pendingClarify = nil
		s.mu.Unlock()
		a.removeKeyboard(m.Chat.ID, prevMsgID)
		a.submitClarifyAnswers(pc, m.Chat.ID)
		return
	}
	s.mu.Unlock()

	// Slash commands.
	switch {
	case text == "/start" || text == "/help":
		a.reply(m.Chat.ID, strings.Join([]string{
			"🤖 Reasonix Telegram",
			"",
			fmt.Sprintf("模式：%s", a.modeLabel()),
			"",
			"指令：",
			"• `/stop` — 中止当前回复",
			"• `/status` — 是否在生成中",
			"• `/new` — 新对话",
			"• `/restart` — 重启桥接",
			"• `/health` — 所有服务状态",
			"• `/sessions` — 活跃会话",
			"• `/chat` — 聊天模式",
			"• `/code` — 编程模式",
			"• `/model` — 切换模型",
			"• `/effort` — 推理深度 (auto/low/medium/high/max)",
			"• 发送 /start 查看本菜单",
			"",
			fmt.Sprintf("单条最多 %d 字，超长自动连发；缓冲约 %d 字节，超时 %d 分钟",
				telegramMaxMessageRunes, a.cfg.MaxOutputBytes, a.cfg.MaxDuration),
		}, "\n"))
		return

	case text == "/stop" || text == "/cancel":
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		t := s.task
		s.mu.Unlock()
		if t == nil {
			a.reply(m.Chat.ID, "当前没有进行中的回复")
			return
		}
		// Signal the goroutine: it will SIGINT the process group, wait up to
		// 5s, then SIGKILL if still alive. s.task is cleared by runTask's defer
		// when the process actually exits, NOT here — so /status stays accurate.
		t.cancel()
		a.reply(m.Chat.ID, "🛑 已发送中止信号")
		return

	case text == "/status":
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		busy := s.task != nil
		model := s.model
		s.mu.Unlock()
		if model == "" {
			model = a.cfg.Model
		}
		modelName, _ := modelByID(model)
		if modelName == "" {
			modelName = model
		}
		// Strip provider prefix: keep only the model name.
		// "custom-opencode-ai: deepseek-v4-flash ⭐" → "deepseek-v4-flash ⭐"
		if idx := strings.LastIndex(modelName, ": "); idx >= 0 {
			modelName = modelName[idx+2:]
		} else if idx := strings.LastIndex(modelName, "/"); idx >= 0 {
			modelName = modelName[idx+1:]
		}
		stateCN := "空闲"
		if busy {
			stateCN = "生成中"
		}
		lines := []string{
			fmt.Sprintf("状态：%s", stateCN),
			fmt.Sprintf("模式：%s", a.modeLabel()),
			fmt.Sprintf("模型：%s", modelName),
		}
		// Usage stats — cumulative session totals.
		s.mu.Lock()
		cumPrompt := s.cumPrompt
		cumCompletion := s.cumCompletion
		cumTotal := s.cumTotal
		cumCost := s.cumCost
		cumCurrency := s.cumCurrency
		// Session-cumulative cache from serve (if available).
		sessHit := s.lastUsage.SessionCacheHitTokens
		sessMiss := s.lastUsage.SessionCacheMissTokens
		sessTotal := sessHit + sessMiss
		s.mu.Unlock()

		if cumTotal > 0 || sessTotal > 0 {
			lines = append(lines, "")
			if cumTotal > 0 {
				lines = append(lines, fmt.Sprintf("输入 %d / 输出 %d / 共 %d tokens", cumPrompt, cumCompletion, cumTotal))
			}
			if sessTotal > 0 {
				hitRate := float64(sessHit) / float64(sessTotal) * 100
				lines = append(lines, fmt.Sprintf("缓存命中：%d / %d（%.2f%%）", sessHit, sessTotal, hitRate))
			}
			if cumCost > 0 {
				lines = append(lines, fmt.Sprintf("总花费：%.4f %s", cumCost, cumCurrency))
			}
		}
		// Context usage from serve.
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port > 0 {
			if used, window := fetchContext(port); window > 0 {
				pct := used * 100 / window
				threshold := 80 // default compact ratio
				left := threshold - pct
				if left < 0 {
					left = 0
				}
				shortUsed := shortTokens(used)
				shortWindow := shortTokens(window)
				lines = append(lines, "")
				if left > 0 {
					lines = append(lines, fmt.Sprintf("上下文：%s / %s（%d%%）· %d%%后压缩", shortUsed, shortWindow, pct, left))
				} else {
					lines = append(lines, fmt.Sprintf("上下文：%s / %s（%d%%）· 即将压缩", shortUsed, shortWindow, pct))
				}
			}
		}
		a.reply(m.Chat.ID, strings.Join(lines, "\n"))
		return

	case text == "/new":
		a.resetReasonixSession(m.Chat.ID)
		a.reply(m.Chat.ID, "🆕 新对话已开启，直接发消息即可。")
		return

	case text == "/restart":
		go a.gracefulServiceRestart(m.Chat.ID)
		return

	case text == "/health":
		a.healthHandler(m)
		return

	case text == "/sessions":
		a.sessionsHandler(m)
		return

	case text == "/chat":
		a.modeHandler(m, "chat")
		return

	case text == "/code":
		a.modeHandler(m, "code")
		return

	case strings.HasPrefix(text, "/model"):
		a.modelHandler(m, strings.TrimSpace(strings.TrimPrefix(text, "/model")))
		return

	case strings.HasPrefix(text, "/effort"):
		a.effortHandler(m, strings.TrimSpace(strings.TrimPrefix(text, "/effort")))
		return

	default:
		if strings.HasPrefix(text, "/") {
			a.reply(m.Chat.ID, fmt.Sprintf("未知指令：%s\n发送 /help 查看可用指令", text))
			return
		}
	}

	// If this is a reply, include the original message as context.
	if m.ReplyToMessage != nil && m.ReplyToMessage.Text != "" {
		text = fmt.Sprintf("[回复消息: %s]\n%s", m.ReplyToMessage.Text, text)
	}

	a.runTask(m.Chat.ID, m.MessageID, text)
}

func (a *App) allowed(u *tgbotapi.User) bool {
	if len(a.cfg.AllowedUsers) == 0 {
		return true
	}
	for _, id := range a.cfg.AllowedUsers {
		if u.ID == id {
			return true
		}
	}
	return false
}

func (a *App) getOrCreateSession(chatID int64) *session {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	if s, ok := a.sess[chatID]; ok {
		return s
	}
	s := &session{
		workdir:     a.chatWorkdir(),
		sessionPath: a.state.sessionPathForChat(chatID),
		servePort:   portForChat(chatID),
	}
	a.sess[chatID] = s
	return s
}

func (a *App) resetReasonixSession(chatID int64) {
	a.stopServe(chatID)
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	path := s.sessionPath
	if path == "" {
		path = a.state.sessionPathForChat(chatID)
	}
	// Reset cumulative usage counters for the new session.
	s.cumPrompt = 0
	s.cumCompletion = 0
	s.cumTotal = 0
	s.cumCost = 0
	s.cumCurrency = ""
	s.lastUsage = wireUsage{}
	s.mu.Unlock()
	_ = os.Remove(path)
	_ = a.state.remove(chatID)
}

func (a *App) reply(chatID int64, text string) {
	if n := a.sendTextParts(chatID, text, nil); n == 0 {
		log.Printf("chat=%d: send reply failed (empty)", chatID)
	}
}

// sendTyping shows the client "typing…" indicator (API returns bool, not Message).
func (a *App) sendTyping(chatID int64) {
	if _, err := a.bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
		log.Printf("chat=%d: sendChatAction typing: %v", chatID, err)
	}
}

// beginTyping shows Telegram "typing…" until the returned stop function runs.
func (a *App) beginTyping(chatID int64) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	send := func() { a.sendTyping(chatID) }
	send()
	go func() {
		ticker := time.NewTicker(typingRefreshInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				send()
			case <-ctx.Done():
				return
			}
		}
	}()
	return cancel
}

// runTask streams the model reply into one Telegram bubble (sendMessageDraft when
// supported, else editMessageText). No "running/done" status prefix — only text.
func (a *App) runTask(chatID int64, replyTo int, prompt string) {
	s := a.getOrCreateSession(chatID)

	s.mu.Lock()
	s.lastActivity = time.Now()
	if t := s.task; t != nil {
		log.Printf("chat=%d: pre-empting running turn", chatID)
		t.cancel()
	}
	s.mu.Unlock()
	a.dismissSessionDraft(chatID)

	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		busy := s.task != nil
		s.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("WARN: chat=%d previous turn didn't exit in 3s", chatID)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := a.ensureServe(chatID); err != nil {
		a.reply(chatID, fmt.Sprintf("Reasonix 服务启动失败: %v", err))
		return
	}

	stopTyping := a.beginTyping(chatID)
	defer stopTyping()

	var ctx context.Context
	var cancel context.CancelFunc
	if a.cfg.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(a.cfg.MaxDuration)*time.Minute)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	s.mu.Lock()
	s.task = &runningTask{cancel: cancel}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.task = nil
		s.wakePusher = nil
		s.mu.Unlock()
		cancel()
	}()

	var (
		buf            strings.Builder
		bufMu          sync.Mutex
		draftMu        sync.Mutex
		truncated      bool
		finished       = make(chan struct{})
		flushNow       = make(chan struct{}, 1) // reasonix "message" / turn_done — finalize early
		pushWake       = make(chan struct{}, 1)
		newSegment     = make(chan struct{}, 1) // tool boundary: finalize + reset
		streamMsgID    int
		draftID        = a.nextDraftID()
		useDraft       = true
		draftShown     bool // sendMessageDraft succeeded for current draftID
		liveDraftEver  bool // any sendMessageDraft succeeded this segment (survives state resets)
		streamDone     bool
		lastDraftBody  string
		msgCreatedAt   time.Time // when first draft/stream msg was sent
		draftFailCount       int  // consecutive draft failures in this turn
		editFailCount        int  // consecutive edit failures in this turn
		streamEditFallback   bool // edit flood-silenced: finalize via sendMessage tail
		streamVisiblePrefix  string // last raw preview successfully shown (edit/draft)
	)
	const (
		maxDraftFailures = 3
		maxEditFailures  = 3
		freshFinalAfter  = 30 * time.Second
		streamDebounce   = 50 * time.Millisecond
	)
	var procErr error
	replyDelivered := false
	releaseTask := func() {
		s.mu.Lock()
		s.task = nil
		s.wakePusher = nil
		s.mu.Unlock()
	}
	// endStream mirrors TelePi finalizeResponse: set streamDone first, flush last
	// draft frame, then sendMessage so no late sendMessageDraft lands after the real message.
	// Fresh final: if the first preview was sent >30s ago, create a new message
	// instead of editing the stale preview (so TG timestamp reflects completion time).
	endStream := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			return
		}
		if draftShown || liveDraftEver {
			a.clearDraftPreview(chatID, draftID)
			draftShown = false
			liveDraftEver = false
		}

		bufMu.Lock()
		raw := buf.String()
		tr := truncated
		bufMu.Unlock()
		body := streamFinalizeBody(raw, lastDraftBody)
		if body != "" && strings.TrimSpace(raw) == "" && strings.TrimSpace(lastDraftBody) != "" {
			log.Printf("chat=%d: endStream using lastDraftBody fallback len=%d", chatID, len(body))
		}
		log.Printf("chat=%d: endStream useDraft=%v draftID=%d bodyLen=%d bodyPreview=%q", chatID, useDraft, draftID, len(body), logPreview(body, 100))
		if body == "" {
			hadPreview := draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody)
			if hadPreview {
				a.clearDraftPreview(chatID, draftID)
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			streamDone = true
			releaseTask()
			log.Printf("chat=%d: endStream body empty, clearedDraft=%v useDraft=%v", chatID, hadPreview, useDraft)
			return
		}
		if len(body) > maxFinalizeBytes {
			body = trimUTF8Bytes(body, maxFinalizeBytes)
			tr = true
		}
		if tr {
			body += "\n\n（内容过长，已截断尾部）"
		}
		// Fresh final: if msgCreatedAt is set and old, send as new message
		// instead of editing the stale one.
		useFreshFinal := !msgCreatedAt.IsZero() && time.Since(msgCreatedAt) > freshFinalAfter
		var n int
		if useDraft && !useFreshFinal {
			n = a.finalizeDraft(chatID, draftID, body, draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody))
			if n > 0 {
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			log.Printf("chat=%d draftID=%d: finalize %d part(s) total=%d runes", chatID, draftID, n, utf8.RuneCountInString(body))
		} else {
			if useFreshFinal && streamMsgID > 0 {
				log.Printf("chat=%d: fresh final (stale preview >%ds), sending new message", chatID, int(freshFinalAfter.Seconds()))
			}
			hadLiveDraft := draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody)
			if streamMsgID > 0 && !useFreshFinal {
				if streamEditFallback {
					tail := streamContinuationText(body, streamVisiblePrefix)
					if tail == "" {
						n = 1
						log.Printf("chat=%d: finalize fallback skip (already shown)", chatID)
					} else {
						log.Printf("chat=%d: finalize fallback send continuation len=%d", chatID, len(tail))
						n = a.sendTextParts(chatID, tail, nil)
					}
				} else {
					streamed := formatForTelegram(telegramPreviewTail(body, telegramMaxMessageRunes))
					if streamed == formatForTelegram(lastDraftBody) {
						n = 1
						log.Printf("chat=%d: finalize skip edit (already shown via stream)", chatID)
					} else {
						editID := streamMsgID
						n = a.sendTextParts(chatID, body, &editID)
					}
				}
			} else {
				n = a.sendTextParts(chatID, body, nil)
			}
			if n > 0 && hadLiveDraft {
				a.clearDraftPreview(chatID, draftID)
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			if n == 0 {
				log.Printf("chat=%d: finalize send failed (0 parts), stream stays open for retry", chatID)
			}
		}
		if n > 0 {
			replyDelivered = true
			streamDone = true
			releaseTask()
		}
	}

	retireLiveDraftLocked := func(reason string) {
		if !draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
			return
		}
		a.clearDraftPreview(chatID, draftID)
		draftShown = false
		liveDraftEver = false
		lastDraftBody = ""
		log.Printf("chat=%d: retired live draft (%s) draftID=%d", chatID, reason, draftID)
	}

	pushDraft := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			log.Printf("chat=%d: pushDraft skip (streamDone)", chatID)
			return
		}
		bufMu.Lock()
		body := strings.TrimSpace(buf.String())
		bufMu.Unlock()
		if body == "" {
			log.Printf("chat=%d: pushDraft skip (empty buffer)", chatID)
			return
		}
		preview := telegramPreviewTail(body, telegramMaxMessageRunes)
		// Native drafts and edit-in-place must not mix: an open sendMessageDraft
		// blocks the user's input even when the preview is invisible. Once we have
		// a stream message to edit, stay on the edit path for this segment.
		if useDraft && streamMsgID == 0 {
			if preview == lastDraftBody {
				return
			}
			if a.sendDraft(chatID, draftID, preview) {
				draftFailCount = 0
				lastDraftBody = preview
				draftShown = true
				liveDraftEver = true
				a.trackSessionDraft(chatID, draftID)
				if msgCreatedAt.IsZero() {
					msgCreatedAt = time.Now()
				}
				return
			}
			draftFailCount++
			if draftFailCount >= maxDraftFailures {
				log.Printf("chat=%d: disabling draft stream after %d failures", chatID, draftFailCount)
			}
			retireLiveDraftLocked("draft_send_failed")
			useDraft = false
			draftID = a.nextDraftID()
		}
		if streamMsgID == 0 {
			previewHTML := formatForTelegram(preview)
			msg := newMessage(chatID, previewHTML)
			msg.ParseMode = "MarkdownV2"
			sent, err := a.sendWithRetry(msg, chatID)
			if err != nil {
				log.Printf("chat=%d: stream send failed: %v", chatID, err)
				editFailCount++
				return
			}
			editFailCount = 0
			streamMsgID = sent.MessageID
			lastDraftBody = preview
			streamVisiblePrefix = preview
			if msgCreatedAt.IsZero() {
				msgCreatedAt = time.Now()
			}
			return
		}
		if streamEditFallback {
			return
		}
		if preview == lastDraftBody {
			return
		}
		previewHTML := formatForTelegram(preview)
		edit := tgbotapi.NewEditMessageText(chatID, streamMsgID, previewHTML)
		edit.ParseMode = "MarkdownV2"
		_, err := a.sendWithRetry(edit, chatID)
		if telegramEditOK(err) {
			editFailCount = 0
			lastDraftBody = preview
			streamVisiblePrefix = preview
			return
		}
		editFailCount++
		if telegramErrorIsFlood(err) || editFailCount >= maxEditFailures {
			streamEditFallback = true
			streamVisiblePrefix = lastDraftBody
			log.Printf("chat=%d: stream edit fallback (flood=%v strikes=%d)", chatID, telegramErrorIsFlood(err), editFailCount)
		}
	}

	signalFlush := func() {
		// Native sendMessageDraft holds the Telegram composer until dismissed — do not
		// wait for finalize/sendMessage; unblock the user as soon as the model finishes.
		a.dismissSessionDraft(chatID)
		select {
		case flushNow <- struct{}{}:
		default:
		}
	}

	wakePush := func() {
		select {
		case pushWake <- struct{}{}:
		default:
		}
	}

	// Register pusher signal on session so clarify answer handlers can kick the stream.
	s.mu.Lock()
	s.wakePusher = wakePush
	s.mu.Unlock()

	go func() {
		defer func() {
			// Belt-and-suspenders: ensure pusher sees a flush even if turn_done
			// onComplete was missed (SSE dropped before turn_done).
			signalFlush()
			close(finished)
		}()
		procErr = a.runServeTurn(ctx, chatID, prompt,
			func(chunk string) {
				bufMu.Lock()
				appendChunk(&buf, chunk, a.cfg.MaxOutputBytes, &truncated)
				bufMu.Unlock()
				wakePush()
			},
			signalFlush,
			func() {
				// onToolDispatch: finalize current text segment and start fresh
				select {
				case newSegment <- struct{}{}:
				default:
				}
			},
			func(text string) int {
				// onCommentary: send a standalone message (tool progress, result)
				// Not part of the stream buffer — send immediately as new message.
				// Don't touch draftMu to avoid contention with pusher goroutine.
				text = capTelegramMessage(text)
				msg := newMessage(chatID, formatForTelegram(text))
				msg.ParseMode = "MarkdownV2"
				sent, err := a.sendWithRetry(msg, chatID)
				if err != nil {
					log.Printf("chat=%d: commentary send failed: %v", chatID, err)
					return 0
				}
				replyDelivered = true
				return sent.MessageID
			},
			func(askID string, questions []askQuestionData) {
				// onAskRequest: model wants user input (ask tool).
				if len(questions) == 0 {
					return
				}

				// Reset stream state so post-answer output can flow in a fresh draft.
				draftMu.Lock()
				if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
					a.clearDraftPreview(chatID, draftID)
				}
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				bufMu.Unlock()
				draftID = a.nextDraftID()
				useDraft = true
				lastDraftBody = ""
				streamMsgID = 0
				msgCreatedAt = time.Now()
				draftMu.Unlock()

				// Build answers map and store all questions for multi-question tracking
				answers := make(map[string][]string, len(questions))

				// Show the FIRST question with buttons
				q := questions[0]
				cidNum := atomic.AddUint64(&a.clarifySeq, 1)
				cid := strconv.FormatUint(cidNum, 36)
				s.mu.Lock()
				s.pendingClarify = &clarifyState{
					question:      q.Text,
					choices:       q.Options,
					askID:         askID,
					questionID:    q.ID,
					port:          s.servePort,
					clarifyID:     cid,
					allQuestions:  questions,
					qIndex:        0,
					answers:       answers,
				}
				s.mu.Unlock()

				// Send question with header + question text + options with descriptions
				qText := _escapeMdv2(strings.TrimSpace(q.Text))
				if qText == "" {
					qText = _escapeMdv2(strings.TrimSpace(q.ID))
				}
				if qText == "" {
					qText = "请选择："
				}
				header := ""
				if len(questions) > 1 {
					header = fmt.Sprintf("问题 1/%d\n", len(questions))
				}
				text := "❓ " + header + qText
				msg := newMessage(chatID, text)
				msg.ParseMode = "MarkdownV2"
				if len(q.Options) > 0 {
					var rows [][]tgbotapi.InlineKeyboardButton
					for i, choice := range q.Options {
						data := fmt.Sprintf("%s%s:%d", prefixClarify, cid, i)
						btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
						rows = append(rows, []tgbotapi.InlineKeyboardButton{
							{Text: btnText, CallbackData: &data},
						})
					}
					otherData := fmt.Sprintf("%s%s:%s", prefixClarify, cid, actionOther)
					rows = append(rows, []tgbotapi.InlineKeyboardButton{
						{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
					})
					msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
				}
				// Send message and store ID for keyboard removal
				if sent, err := a.sendWithRetry(msg, chatID); err != nil {
					log.Printf("send failed: %v", err)
				} else {
					s.mu.Lock()
					s.pendingClarify.messageID = sent.MessageID
					s.mu.Unlock()
				}
				replyDelivered = true
			},
			func(approvalID, toolName string) {
				// onApprovalRequest: model needs user approval (plan or tool).
				// Finalize current stream content first.
				signalFlush()
				// Reset stream state so post-approval output can flow in a fresh draft.
				draftMu.Lock()
				if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
					a.clearDraftPreview(chatID, draftID)
				}
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				bufMu.Unlock()
				draftID = a.nextDraftID()
				useDraft = true
				lastDraftBody = ""
				streamMsgID = 0
				msgCreatedAt = time.Now()
				draftMu.Unlock()
				replyDelivered = true

				// Show approval prompt with inline buttons
				var label string
				var emoji string
				switch toolName {
				case "plan":
					label = "执行计划"
					emoji = "📋"
				default:
					label = toolName
					emoji = "🔧"
				}

				// Set pendingApproval for callback
				s.mu.Lock()
				apID := fmt.Sprintf("%s%s", prefixApproval, approvalID)
				s.pendingApproval = &approvalState{
					approvalID: approvalID,
					toolName:   toolName,
					port:       s.servePort,
				}
				s.mu.Unlock()

				text := fmt.Sprintf("%s 需要批准：%s", emoji, _escapeMdv2(label))
				onceData := fmt.Sprintf("%s:%s", apID, actionOnce)
				sessionData := fmt.Sprintf("%s:%s", apID, actionSession)
				denyData := fmt.Sprintf("%s:%s", apID, actionDeny)
				msg := newMessage(chatID, text)
				msg.ParseMode = "MarkdownV2"
				msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					[]tgbotapi.InlineKeyboardButton{
						{Text: "✅ 批准一次", CallbackData: &onceData},
						{Text: "🔒 始终批准", CallbackData: &sessionData},
					},
					[]tgbotapi.InlineKeyboardButton{
						{Text: "❌ 拒绝", CallbackData: &denyData},
					},
				)
				a.sendSafe(msg)
			},
			func(u wireUsage) {
				// onUsage: accumulate session totals + store latest for /status.
				s.mu.Lock()
				s.lastUsage = u
				s.cumPrompt += u.PromptTokens
				s.cumCompletion += u.CompletionTokens
				s.cumTotal += u.TotalTokens
				s.cumCost += u.Cost
				if u.Currency != "" {
					s.cumCurrency = u.Currency
				}
				s.mu.Unlock()
			},
		)
	}()

	pusherDone := make(chan struct{})
	go func() {
		defer close(pusherDone)
		debounce := time.NewTimer(time.Hour)
		debounce.Stop()
		stopDebounce := func() {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
		}
		flushAndEnd := func() {
			stopDebounce()
			pushDraft()
			endStream()
		}
		// newSegmentHandler finalizes the current text as a complete message,
		// resets the buffer, and continues streaming in a new bubble.
		// Used at tool boundaries (tool mode only).
		newSegmentHandler := func() {
			stopDebounce()
			// Tool boundary: keep streaming even if a stray onComplete slipped through.
			draftMu.Lock()
			streamDone = false
			segDraftID := draftID
			segDraftShown := draftShown
			segLiveDraftEver := liveDraftEver
			segUseDraft := useDraft
			segStreamMsgID := streamMsgID
			draftMu.Unlock()
			pushDraft()
			draftMu.Lock()
			bufMu.Lock()
			body := strings.TrimSpace(buf.String())
			buf.Reset()
			truncated = false
			bufMu.Unlock()
			segHadLiveDraft := draftNeedsCleanup(segDraftShown || draftShown, segLiveDraftEver || liveDraftEver, lastDraftBody)
			if body != "" {
				if segUseDraft {
					a.finalizeDraft(chatID, segDraftID, body, segHadLiveDraft)
				} else if segStreamMsgID > 0 {
					a.sendTextParts(chatID, body, &segStreamMsgID)
				} else {
					a.sendTextParts(chatID, body, nil)
				}
				replyDelivered = true
			}
			if segHadLiveDraft {
				a.clearDraftPreview(chatID, segDraftID)
			}
			// Post-tool segments use edit-in-place, not native drafts — an open
			// sendMessageDraft blocks the user from replying until Telegram times it out.
			draftID = a.nextDraftID()
			useDraft = false
			draftShown = false
			liveDraftEver = false
			lastDraftBody = ""
			streamMsgID = 0
			streamEditFallback = false
			streamVisiblePrefix = ""
			msgCreatedAt = time.Now()
			draftMu.Unlock()
		}
		drainFlush := func() bool {
			select {
			case <-flushNow:
				log.Printf("chat=%d: pusher: flushNow", chatID)
				flushAndEnd()
				return true
			default:
				return false
			}
		}
		for {
			// turn_done signals flushNow; drain it before finished so we never
			// mark streamDone on an empty pre-empt while content is still pending.
			if drainFlush() {
				continue
			}
			select {
			case <-pushWake:
				log.Printf("chat=%d: pusher: pushWake", chatID)
				stopDebounce()
				debounce.Reset(streamDebounce)
			case <-debounce.C:
				log.Printf("chat=%d: pusher: debounce fire", chatID)
				pushDraft()
			case <-newSegment:
				log.Printf("chat=%d: pusher: newSegment", chatID)
				newSegmentHandler()
			case <-flushNow:
				log.Printf("chat=%d: pusher: flushNow", chatID)
				flushAndEnd()
			case <-finished:
				log.Printf("chat=%d: pusher: finished", chatID)
				if drainFlush() {
					continue
				}
				flushAndEnd()
				draftMu.Lock()
				done := streamDone
				draftMu.Unlock()
				if done {
					return
				}
				// runServeTurn returned before turn_done flush; wait briefly for it.
				select {
				case <-flushNow:
					log.Printf("chat=%d: pusher: late flushNow after finished", chatID)
					flushAndEnd()
				case <-time.After(3 * time.Second):
					log.Printf("chat=%d: pusher: finished without finalize, giving up", chatID)
				}
				return
			}
		}
	}()

	select {
	case <-pusherDone:
	case <-ctx.Done():
		select {
		case <-pusherDone:
		case <-time.After(8 * time.Second):
			if procErr == nil {
				procErr = ctx.Err()
			}
		}
	}

	if procErr != nil {
		draftMu.Lock()
		if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
			a.clearDraftPreview(chatID, draftID)
			draftShown = false
			liveDraftEver = false
		}
		streamDone = true
		draftMu.Unlock()
		if replyDelivered && errors.Is(procErr, context.Canceled) {
			log.Printf("chat=%d prompt=%q: canceled after reply delivered (draft cleared)", chatID, logPreview(prompt, 80))
			return
		}
		msg := fmt.Sprintf("请求失败：%s", userFacingError(procErr))
		if errors.Is(procErr, context.DeadlineExceeded) {
			msg = fmt.Sprintf("超时（%d 分钟）", a.cfg.MaxDuration)
		} else if errors.Is(procErr, context.Canceled) {
			msg = "已中止"
		}
		a.reply(chatID, msg)
		log.Printf("chat=%d prompt=%q err=%v", chatID, logPreview(prompt, 80), procErr)
		return
	}

	bufMu.Lock()
	empty := strings.TrimSpace(buf.String()) == ""
	bufMu.Unlock()

	// Silence detection: if the only reply was silence narration, suppress it.
	if !empty && isSilenceOnly(buf.String()) {
		log.Printf("chat=%d: suppressed silence-only reply", chatID)
		empty = true
	}

	log.Printf("chat=%d: finalCheck empty=%v replyDelivered=%v procErr=%v", chatID, empty, replyDelivered, procErr)
	if empty && !replyDelivered {
		a.reply(chatID, "（模型处理完成，但没有生成可见回复。请再发一次或换种问法。）")
	}
	bufMu.Lock()
	finalBody := strings.TrimSpace(buf.String())
	bufMu.Unlock()
	log.Printf("chat=%d prompt=%q stream=draft draftID=%d finalLen=%d runes=%d body=%q",
		chatID, logPreview(prompt, 80), draftID, len(finalBody), utf8.RuneCountInString(finalBody), logPreview(finalBody, 200))
}

// streamFinalizeBody picks the text to finalize at turn end. The accumulator
// buffer can lag behind or be reset while sendMessageDraft already shows text
// in lastDraftBody — falling back prevents a stuck draft with no sendMessage.
func streamFinalizeBody(buf, lastDraftBody string) string {
	body := strings.TrimSpace(buf)
	if body == "" {
		body = strings.TrimSpace(lastDraftBody)
	}
	return body
}

// nextDraftID returns a unique Telegram draft_id (int32-safe, no second-level collisions).
func (a *App) nextDraftID() int64 {
	seq := atomic.AddUint64(&a.draftSeq, 1)
	// Low 9 digits from unix seconds + 4-digit sequence within the same second.
	return int64(time.Now().Unix()%1_000_000_000)*10000 + int64(seq%10000)
}

func draftHadPreview(lastDraftBody string) bool {
	return strings.TrimSpace(lastDraftBody) != ""
}

func draftNeedsCleanup(draftShown, liveDraftEver bool, lastDraftBody string) bool {
	_ = lastDraftBody // edit-in-place preview; not a native sendMessageDraft
	return draftShown || liveDraftEver
}

// clearDraftPreview retires a live sendMessageDraft bubble. Only safe when a
// non-empty preview was previously sent for this draft_id — empty dismiss on a
// never-shown draft creates a brief ghost bubble on Telegram.
func (a *App) clearDraftPreview(chatID int64, draftID int64) {
	if draftID == 0 {
		return
	}
	a.dismissDraft(chatID, draftID)
	a.clearSessionDraft(chatID, draftID)
	log.Printf("chat=%d draftID=%d: cleared draft preview", chatID, draftID)
}

// finalizeDraft ends a native-draft segment with sendMessage (Hermes pattern).
// sendMessage first so a failed HTML format does not dismiss the live preview;
// dismiss the draft only after the real message lands.
func (a *App) finalizeDraft(chatID int64, draftID int64, text string, hadLiveDraft bool) int {
	if strings.TrimSpace(text) == "" {
		if hadLiveDraft {
			a.clearDraftPreview(chatID, draftID)
		}
		return 0
	}
	if hadLiveDraft {
		a.clearDraftPreview(chatID, draftID)
	}
	return a.sendTextParts(chatID, text, nil)
}

func (a *App) trackSessionDraft(chatID int64, draftID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	s.liveDraftID = draftID
	s.mu.Unlock()
}

func (a *App) clearSessionDraft(chatID int64, draftID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.liveDraftID == draftID {
		s.liveDraftID = 0
	}
	s.mu.Unlock()
}

func (a *App) dismissSessionDraft(chatID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	draftID := s.liveDraftID
	s.liveDraftID = 0
	s.mu.Unlock()
	if draftID == 0 {
		return
	}
	a.dismissDraft(chatID, draftID)
	log.Printf("chat=%d: dismissed session draftID=%d (pre-empt/stale cleanup)", chatID, draftID)
}

// sendDraft pushes streaming preview text via sendMessageDraft (Bot API 9.5+).
// Text is automatically converted from markdown to Telegram HTML format.
func (a *App) sendDraft(chatID int64, draftID int64, text string) bool {
	text = formatForTelegram(text)
	text = telegramPreviewTail(text, telegramMaxMessageRunes)
	if text == "" {
		return false
	}
	_, err := a.bot.MakeRequest("sendMessageDraft", tgbotapi.Params{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"draft_id":   strconv.FormatInt(draftID, 10),
		"text":       text,
		"parse_mode": "MarkdownV2",
	})
	if err != nil {
		log.Printf("sendMessageDraft failed (fallback to edit): %v", err)
		return false
	}
	return true
}

// dismissDraft clears a native draft preview by sending an empty sendMessageDraft.
func (a *App) dismissDraft(chatID int64, draftID int64) {
	_, _ = a.bot.MakeRequest("sendMessageDraft", tgbotapi.Params{
		"chat_id":  strconv.FormatInt(chatID, 10),
		"draft_id": strconv.FormatInt(draftID, 10),
		"text":     "",
	})
}

func (a *App) healthHandler(m *tgbotapi.Message) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	lines := []string{fmt.Sprintf("模式: %s", a.modeLabel())}
	if len(a.sess) == 0 {
		lines = append(lines, "暂无活跃会话")
	} else {
		for chatID, s := range a.sess {
			s.mu.Lock()
			busy := s.task != nil
			running := s.serveCmd != nil && s.serveCmd.Process != nil && s.serveCmd.ProcessState == nil
			s.mu.Unlock()
			status := "🟢 运行中"
			if !running {
				status = "🔴 已停止"
			} else if busy {
				status = "🟡 生成中"
			}
			lines = append(lines, fmt.Sprintf("聊天 %d: %s", chatID, status))
		}
	}
	a.reply(m.Chat.ID, strings.Join(lines, "\n"))
}

func (a *App) modeHandler(m *tgbotapi.Message, arg string) {
	// Block during restart, same as handleMessage.
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	var newMode string
	switch arg {
	case "chat", "":
		newMode = ModeChat
	case "code", "tool":
		newMode = ModeTool
	default:
		a.reply(m.Chat.ID, "用法：/chat 或 /code")
		return
	}
	if a.getMode() == newMode {
		a.reply(m.Chat.ID, fmt.Sprintf("当前已经是%s", modeLabelFor(newMode)))
		return
	}
	// Stop existing serve, switch mode, rewrite toml, restart.
	a.stopServe(m.Chat.ID)
	a.setMode(newMode)
	_ = a.ensureChatWorkdir()
	if err := a.startServe(m.Chat.ID); err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("切换模式失败: %v", err))
		return
	}
	a.reply(m.Chat.ID, fmt.Sprintf("已切换到%s", modeLabelFor(newMode)))
}

func (a *App) modelHandler(m *tgbotapi.Message, arg string) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		a.sendModelPicker(m.Chat.ID, 0)
		return
	}

	name, ok := modelByID(arg)
	if !ok {
		ids := make([]string, len(availableModels))
		for i, m := range availableModels {
			ids[i] = m.ID
		}
		a.reply(m.Chat.ID, fmt.Sprintf("未知模型：%s\n可用：%s", arg, strings.Join(ids, ", ")))
		return
	}

	s := a.getOrCreateSession(m.Chat.ID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = a.cfg.Model
	}
	s.mu.Unlock()

	if arg == current {
		a.reply(m.Chat.ID, fmt.Sprintf("当前已经是 %s", name))
		return
	}

	a.switchModel(m.Chat.ID, arg, name)
}

const modelsPerPage = 4

func (a *App) sendModelPicker(chatID int64, page int) {
	total := len(availableModels)
	totalPages := (total + modelsPerPage - 1) / modelsPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * modelsPerPage
	end := start + modelsPerPage
	if end > total {
		end = total
	}

	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = a.cfg.Model
	}
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		m := availableModels[i]
		label := m.Name
		if m.ID == current {
			label = "✅ " + label
		}
		data := fmt.Sprintf("%s%s", prefixModel, m.ID)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: label, CallbackData: &data},
		})
	}

	// Pagination row
	if totalPages > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			pdata := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page-1)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "◀️ 上一页", CallbackData: &pdata})
		}
		nav = append(nav, tgbotapi.InlineKeyboardButton{Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: strPtr("_")})
		if page < totalPages-1 {
			pdata := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page+1)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "下一页 ▶️", CallbackData: &pdata})
		}
		rows = append(rows, nav)
	}

	text := fmt.Sprintf("🤖 选择模型（当前：%s）", a.modelDisplayName(current))
	msg := newMessage(chatID, text)
	msg.ParseMode = "MarkdownV2"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendSafe(msg)
}

// editModelPicker edits an existing picker message with a new page of models.
func (a *App) editModelPicker(chatID int64, messageID int, page int) {
	total := len(availableModels)
	totalPages := (total + modelsPerPage - 1) / modelsPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * modelsPerPage
	end := start + modelsPerPage
	if end > total {
		end = total
	}

	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = a.cfg.Model
	}
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		m := availableModels[i]
		label := m.Name
		if m.ID == current {
			label = "✅ " + label
		}
		data := fmt.Sprintf("%s%s", prefixModel, m.ID)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: label, CallbackData: &data},
		})
	}

	// Pagination row
	if totalPages > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			pdata := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page-1)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "◀️ 上一页", CallbackData: &pdata})
		}
		nav = append(nav, tgbotapi.InlineKeyboardButton{Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: strPtr("_")})
		if page < totalPages-1 {
			pdata := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page+1)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "下一页 ▶️", CallbackData: &pdata})
		}
		rows = append(rows, nav)
	}

	text := fmt.Sprintf("🤖 选择模型（当前：%s）", a.modelDisplayName(current))
	edit := tgbotapi.NewEditMessageText(chatID, messageID, text)
	edit.ParseMode = "MarkdownV2"
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if _, err := a.bot.Request(edit); err != nil {
		log.Printf("edit model picker failed: %v", err)
	}
}

func strPtr(s string) *string { return &s }

func (a *App) modelDisplayName(id string) string {
	if name, ok := modelByID(id); ok {
		return name
	}
	return id
}

// fetchContext queries the reasonix serve /context endpoint for used/window tokens.
func fetchContext(port int) (used, window int) {
	resp, err := http.Get(serveBaseURL(port) + "/context")
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	var c struct {
		Used   int `json:"used"`
		Window int `json:"window"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return 0, 0
	}
	return c.Used, c.Window
}

// shortTokens formats a token count as 1.2K, 142.0K, 1.0M, etc.
func shortTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

func (a *App) switchModel(chatID int64, modelID, modelName string) {
	s := a.getOrCreateSession(chatID)
	a.stopServe(chatID)
	s.mu.Lock()
	s.model = modelID
	s.mu.Unlock()
	// Persist to .env file.
	if err := a.persistModel(modelID); err != nil {
		log.Printf("chat=%d: persist model failed: %v", chatID, err)
	}
	if err := a.startServe(chatID); err != nil {
		a.reply(chatID, fmt.Sprintf("切换模型失败: %v", err))
		return
	}
	a.reply(chatID, fmt.Sprintf("已切换到 %s", modelName))
}

var effortLevels = []struct {
	ID   string
	Name string
}{
	{"auto", "自动 (默认)"},
	{"low", "低"},
	{"medium", "中"},
	{"high", "高"},
	{"max", "最高"},
}

func (a *App) effortHandler(m *tgbotapi.Message, arg string) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		// Ensure serve is running so we have a port.
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port == 0 {
			if err := a.startServe(m.Chat.ID); err != nil {
				a.reply(m.Chat.ID, fmt.Sprintf("启动 serve 失败: %v", err))
				return
			}
		}
		a.sendEffortPicker(m.Chat.ID, 0)
		return
	}

	// Validate level.
	valid := false
	for _, l := range effortLevels {
		if l.ID == arg {
			valid = true
			break
		}
	}
	if !valid {
		a.reply(m.Chat.ID, "用法：/effort auto|low|medium|high|max")
		return
	}

	// Submit to reasonix serve.
	s := a.getOrCreateSession(m.Chat.ID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()
	if port == 0 {
		a.reply(m.Chat.ID, "serve 未运行")
		return
	}
	if err := postJSON(port, "/submit", map[string]string{"input": "/effort " + arg}); err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("切换 effort 失败: %v", err))
		return
	}
	a.reply(m.Chat.ID, fmt.Sprintf("推理深度已切换到 %s", arg))
}

func (a *App) sendEffortPicker(chatID int64, _ int) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, l := range effortLevels {
		data := fmt.Sprintf("%s%s", prefixEffort, l.ID)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: l.Name, CallbackData: &data},
		})
	}
	msg := newMessage(chatID, "🤔 选择推理深度")
	msg.ParseMode = "MarkdownV2"
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendSafe(msg)
	_ = port // keep port reference for future use
}

// persistModel writes the model ID to the .env file so it survives restarts.
func (a *App) persistModel(modelID string) error {
	envPath := filepath.Join(a.cfg.StateDir, "env")
	data := []byte("MODEL=" + modelID + "\n")
	return os.WriteFile(envPath, data, 0o600)
}

func (a *App) handleCallbackQuery(cq *tgbotapi.CallbackQuery) {
	if cq.Message == nil || cq.From == nil {
		return
	}
	chatID := cq.Message.Chat.ID
	data := strings.TrimSpace(cq.Data)
	log.Printf("chat=%d: callback data=%q", chatID, data)

	// --- Approval callbacks: ap:{approvalID}:{action} ---
	if strings.HasPrefix(data, prefixApproval) {
		parts := strings.SplitN(data, ":", 3)
		if len(parts) < 3 {
			return
		}
		approvalID := parts[1]
		action := parts[2]

		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		pa := s.pendingApproval
		if pa == nil || pa.approvalID != approvalID {
			s.mu.Unlock()
			return
		}
		s.pendingApproval = nil
		s.mu.Unlock()

		a.answerCallback(cq.ID, "")
		a.removeKeyboard(chatID, cq.Message.MessageID)

		var allow bool
		var session bool
		switch action {
		case actionOnce:
			allow = true
			session = false
		case actionSession:
			allow = true
			session = true
		default: // deny
			allow = false
			session = false
		}

		_ = postJSON(pa.port, "/approve", map[string]any{
			"id": approvalID, "allow": allow, "session": session,
		})
		log.Printf("chat=%d: approval %s -> allow=%v session=%v", chatID, approvalID, allow, session)
		return
	}

	// --- Model callbacks: md:{modelID} or md:page:{page} ---
	if strings.HasPrefix(data, prefixModel) {
		a.answerCallback(cq.ID, "")
		payload := strings.TrimPrefix(data, prefixModel)
		if payload == "_" {
			return // no-op (page indicator)
		}
		if strings.HasPrefix(payload, actionPage+":") {
			// Pagination — edit current message instead of sending a new one
			pageStr := strings.TrimPrefix(payload, actionPage+":")
			page, _ := strconv.Atoi(pageStr)
			a.editModelPicker(chatID, cq.Message.MessageID, page)
			return
		}
		// Model selection
		modelID := payload
		name, ok := modelByID(modelID)
		if !ok {
			return
		}
		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		current := s.model
		if current == "" {
			current = a.cfg.Model
		}
		s.mu.Unlock()
		// Delete the picker message entirely — bubble + buttons both gone.
		a.deleteMessage(chatID, cq.Message.MessageID)
		if modelID == current {
			a.reply(chatID, fmt.Sprintf("当前已经是 %s", name))
			return
		}
		a.switchModel(chatID, modelID, name)
		return
	}

	// --- Effort callbacks: ef:{level} ---
	if strings.HasPrefix(data, prefixEffort) {
		a.answerCallback(cq.ID, "")
		level := strings.TrimPrefix(data, prefixEffort)
		if level == "" {
			return
		}

		// Get serve port from session.
		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port == 0 {
			a.reply(chatID, "serve 未运行")
			return
		}
		if err := postJSON(port, "/submit", map[string]string{"input": "/effort " + level}); err != nil {
			a.reply(chatID, fmt.Sprintf("切换 effort 失败: %v", err))
			return
		}
		a.removeKeyboard(chatID, cq.Message.MessageID)
		a.reply(chatID, fmt.Sprintf("推理深度已切换到 %s", level))
		return
	}

	// --- Clarify callbacks: cl:{clarifyID}:{choice} ---
	if !strings.HasPrefix(data, prefixClarify) {
		return
	}
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 {
		return
	}
	clarifyID := parts[1]
	choiceIdx := parts[2]

	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	pc := s.pendingClarify
	if pc == nil || pc.clarifyID != clarifyID {
		s.mu.Unlock()
		return
	}

	if choiceIdx == actionOther {
		log.Printf("chat=%d: clarify 'other' clicked, pendingClarify=%v", chatID, pc != nil)
		pc.awaitingCustom = true
		msgID := pc.messageID
		s.mu.Unlock()
		a.answerCallback(cq.ID, "")
		a.removeKeyboard(chatID, msgID)
		msg := newMessage(chatID, "📝 请输入你的回答：")
		msg.ParseMode = "MarkdownV2"
		msg.ReplyToMessageID = cq.Message.MessageID
		a.sendSafe(msg)
		return
	}

	// Resolve button choice to answer text.
	pc.awaitingCustom = false
	var answer string
	if idx, err := strconv.Atoi(choiceIdx); err == nil && idx >= 0 && idx < len(pc.choices) {
		answer = pc.choices[idx]
	}
	if answer == "" {
		answer = choiceIdx
	}

	// Store answer and advance (all under lock to avoid concurrent-map write).
	pc.answers[pc.questionID] = []string{answer}
	nextIdx := pc.qIndex + 1
	if nextIdx < len(pc.allQuestions) {
		nextQ := pc.allQuestions[nextIdx]
		pc.qIndex = nextIdx
		pc.questionID = nextQ.ID
		pc.choices = nextQ.Options
		cidNum := atomic.AddUint64(&a.clarifySeq, 1)
		pc.clarifyID = strconv.FormatUint(cidNum, 36)
		// Snapshot data for message building.
		qText := _escapeMdv2(strings.TrimSpace(nextQ.Text))
		if qText == "" {
			qText = _escapeMdv2(strings.TrimSpace(nextQ.ID))
		}
		if qText == "" {
			qText = "请选择："
		}
		options := nextQ.Options
		clarifyID := pc.clarifyID
		prevMsgID := pc.messageID
		s.mu.Unlock()

		a.removeKeyboard(chatID, prevMsgID)

		header := fmt.Sprintf("问题 %d/%d\n", nextIdx+1, len(pc.allQuestions))
		text := "❓ " + header + qText
		msg := newMessage(chatID, text)
		msg.ParseMode = "MarkdownV2"
		if len(options) > 0 {
			var rows [][]tgbotapi.InlineKeyboardButton
			for i, choice := range options {
				data := fmt.Sprintf("%s%s:%d", prefixClarify, clarifyID, i)
				btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
				rows = append(rows, []tgbotapi.InlineKeyboardButton{
					{Text: btnText, CallbackData: &data},
				})
			}
			otherData := fmt.Sprintf("%s%s:%s", prefixClarify, clarifyID, actionOther)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
			})
			msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
		}
		if sent, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("send failed: %v", err)
		} else {
			s.mu.Lock()
			s.pendingClarify.messageID = sent.MessageID
			s.mu.Unlock()
		}
		a.answerCallback(cq.ID, "")
		return
	}

	// All questions answered — submit.
	prevMsgID := pc.messageID
	s.pendingClarify = nil
	s.mu.Unlock()
	a.removeKeyboard(chatID, prevMsgID)

	a.answerCallback(cq.ID, "")
	a.submitClarifyAnswers(pc, chatID)
}

func (a *App) formatChoices(choices []string) string {
	var b strings.Builder
	for i, c := range choices {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, _escapeMdv2(c))
	}
	return b.String()
}

func (a *App) sessionsHandler(m *tgbotapi.Message) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	var lines []string
	if len(a.sess) == 0 {
		lines = append(lines, "暂无活跃会话")
	} else {
		for chatID, s := range a.sess {
			s.mu.Lock()
			la := s.lastActivity
			s.mu.Unlock()
			line := fmt.Sprintf("聊天 %d", chatID)
			if !la.IsZero() {
				line += fmt.Sprintf(" · 最后活跃 %s 前", time.Since(la).Round(time.Second))
			}
			lines = append(lines, line)
		}
	}
	a.reply(m.Chat.ID, strings.Join(lines, "\n"))
}

// killDescendants SIGKILLs any process whose ppid is the given pid, walking
// recursively. Best-effort: relies on /proc being readable, which is true on
// Linux. We don't fail loudly if /proc is missing or the pid is gone.
func killDescendants(rootPid int) {
	visited := map[int]bool{rootPid: true}
	var walk func(int)
	walk = func(ppid int) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pid, err := strconv.Atoi(e.Name())
			if err != nil || visited[pid] {
				continue
			}
			statBytes, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			// stat format: "pid (comm) state ppid pgrp ..."
			// comm can contain spaces and parens; the safe way is to find
			// the LAST ')' and parse from there.
			stat := string(statBytes)
			rpar := strings.LastIndex(stat, ")")
			if rpar < 0 || rpar+2 >= len(stat) {
				continue
			}
			fields := strings.Fields(stat[rpar+2:])
			if len(fields) < 2 {
				continue
			}
			// fields[0] = state, fields[1] = ppid
			parent, err := strconv.Atoi(fields[1])
			if err != nil {
				continue
			}
			if parent == ppid {
				visited[pid] = true
				_ = syscall.Kill(pid, syscall.SIGKILL)
				walk(pid)
			}
		}
	}
	walk(rootPid)
}


// sendSafe sends a message with retry and logs any error.
func (a *App) sendSafe(msg tgbotapi.Chattable) {
	if _, err := a.sendWithRetry(msg, 0); err != nil {
		log.Printf("send failed: %v", err)
	}
}

// answerCallback answers a callback query and logs any error.
func (a *App) answerCallback(id, text string) {
	if _, err := a.bot.Request(tgbotapi.NewCallback(id, text)); err != nil {
		log.Printf("callback answer failed: %v", err)
	}
}

// removeKeyboard removes the inline keyboard from a message (call after user selects).
func (a *App) removeKeyboard(chatID int64, messageID int) {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
	})
	if _, err := a.bot.Request(edit); err != nil && !telegramErrorIsNotModified(err) {
		log.Printf("remove keyboard failed: %v", err)
	}
}

// deleteMessage deletes a message entirely.
func (a *App) deleteMessage(chatID int64, messageID int) {
	if _, err := a.bot.Request(tgbotapi.NewDeleteMessage(chatID, messageID)); err != nil {
		log.Printf("delete message failed: %v", err)
	}
}

// editCommentary updates a tool-dispatch message. Caps length and treats
// "message is not modified" as success.
func (a *App) editCommentary(chatID int64, messageID int, appendText string) error {
	text := capTelegramMessage(appendText)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, formatForTelegram(text))
	edit.ParseMode = "MarkdownV2"
	_, err := a.sendWithRetry(edit, chatID)
	if telegramEditOK(err) {
		return nil
	}
	log.Printf("chat=%d: edit commentary failed: %v", chatID, err)
	return err
}

// appendToCommentary replaces a tool-dispatch message (same caps as editCommentary).
func (a *App) appendToCommentary(chatID int64, messageID int, appendText string) error {
	return a.editCommentary(chatID, messageID, appendText)
}

// submitClarifyAnswers POSTs accumulated clarify answers to the serve backend.
func (a *App) submitClarifyAnswers(pc *clarifyState, chatID int64) {
	type answerEntry struct {
		QuestionID string   `json:"questionId"`
		Selected   []string `json:"selected"`
	}
	var answersPayload []answerEntry
	for _, q := range pc.allQuestions {
		sel, ok := pc.answers[q.ID]
		if !ok {
			sel = []string{""}
		}
		answersPayload = append(answersPayload, answerEntry{
			QuestionID: q.ID,
			Selected:   sel,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id":      pc.askID,
		"answers": answersPayload,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/answer", pc.port)
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("chat=%d: post answer failed: %v", chatID, err)
		return
	}
	resp.Body.Close()
	log.Printf("chat=%d: all %d answers submitted to serve (askID=%s)", chatID, len(pc.allQuestions), pc.askID)

	// Kick the turn pusher: model will continue emitting events on the existing SSE stream.
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.wakePusher != nil {
		s.wakePusher()
	}
	s.mu.Unlock()
}

// userFacingError maps known Reasonix errors to short Chinese for Telegram users.
func userFacingError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "paused after"):
		return "本轮已暂停（达到步数上限），可再发一条消息继续"
	case strings.Contains(s, "reasonix serve not ready"):
		return "Reasonix 未就绪，请稍后重试或发送 /restart"
	case strings.HasPrefix(s, "submit:"):
		return "提交失败：" + strings.TrimSpace(strings.TrimPrefix(s, "submit:"))
	case strings.Contains(s, "connection refused"):
		return "无法连接 Reasonix 服务"
	default:
		return s
	}
}


