// reasonix-telegram: Telegram bridge for Reasonix (DeepSeek-Reasonix coding agent).
// - Keeps one long-lived `reasonix serve` per Telegram chat (multi-turn session)
// - Persists session files under /var/lib/reasonix-telegram; restores on bridge restart
// - Streams replies via Telegram draft or edit-in-place (no status-line prefix)
// - Chat-only mode: dedicated workdir + reasonix.toml disables all tools
// - /new starts a fresh Reasonix session
// - Configurable max output length, allow-listed users (optional)
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
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
	if strings.TrimSpace(line) == "" {
		return false // preserve blank lines; the scanner trims these implicitly
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
	n, _ := strconv.Atoi(s)
	return n
}

// session: one per Telegram chat — workdir, Reasonix session file, serve process.
type session struct {
	mu           sync.Mutex
	workdir      string
	sessionPath  string
	servePort    int
	serveCmd     *exec.Cmd
	task         *runningTask // non-nil while a turn is in flight
	lastActivity time.Time    // last message activity, for /sessions
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
	draftSeq       uint64 // per-process draft_id sequence (avoids same-second collisions)
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

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("telegram auth failed: %v", err)
	}
	bot.Debug = false
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
	if err := app.ensureChatWorkdir(); err != nil {
		log.Fatalf("chat workdir: %v", err)
	}
	log.Printf("mode=%s workdir=%s", app.cfg.Mode, app.chatWorkdir())
	log.Printf("telegram stream: sendMessageDraft + sendMessage finalize (TelePi/Hermes pattern)")
	app.startRestartWatchdog()
	app.restorePersistedSessions()
	app.notifyBridgeRestarted()

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("shutdown signal: flushing reasonix sessions…")
		app.cancelAllTasks()
		app.waitTasksDone(5 * time.Second)
		app.stopAllServes()
		os.Exit(0)
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	for upd := range updates {
		if upd.Message == nil {
			continue
		}
		go app.handleMessage(upd.Message)
	}
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
		{Command: "clear", Description: "同 /new，开启新对话"},
		{Command: "restart", Description: "重启桥接"},
		{Command: "health", Description: "所有 serve 进程状态"},
		{Command: "sessions", Description: "活跃会话列表"},
		{Command: "mode", Description: "切换模式：/mode chat 或 /mode tool"},
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
		a.reply(m.Chat.ID, "🔄 服务重启中，完成后会发 🟢 已连接 提示。")
		return
	}
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return
	}

	// Slash commands.
	switch {
	case text == "/start" || text == "/help":
		modeLabel := "纯对话（工具关闭）"
		if a.cfg.Mode == ModeTool {
			modeLabel = "全能编程（工具可用）"
		}
		a.reply(m.Chat.ID, strings.Join([]string{
			"🤖 Reasonix Telegram",
			"",
			fmt.Sprintf("模式：%s", modeLabel),
			"",
			"指令：",
			"• `/stop` — 中止当前回复",
			"• `/status` — 是否在生成中",
			"• `/new`、`/clear` — 新对话",
			"• `/restart` — 重启桥接",
			"• `/health` — 所有服务状态",
			"• `/sessions` — 活跃会话",
			"• `/mode chat|tool` — 切换模式",
			"• 直接发消息 — 开始对话",
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
		s.mu.Unlock()
		stateCN := "空闲"
		if busy {
			stateCN = "生成中"
		}
		modeLabel := "纯对话（工具关闭）"
		if a.cfg.Mode == ModeTool {
			modeLabel = "全能编程（工具可用）"
		}
		a.reply(m.Chat.ID, fmt.Sprintf("状态：%s\n模式：%s", stateCN, modeLabel))
		return

	case text == "/new" || text == "/clear":
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

	case strings.HasPrefix(text, "/mode"):
		a.modeHandler(m, strings.TrimSpace(strings.TrimPrefix(text, "/mode")))
		return
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

	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		busy := s.task != nil
		s.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("WARN: chat=%d previous turn didn't exit in 10s", chatID)
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := a.ensureServe(chatID); err != nil {
		a.reply(chatID, fmt.Sprintf("Reasonix 服务启动失败: %v", err))
		return
	}

	stopTyping := a.beginTyping(chatID)
	defer stopTyping()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.cfg.MaxDuration)*time.Minute)
	s.mu.Lock()
	s.task = &runningTask{cancel: cancel}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.task = nil
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
		streamDone     bool
		lastDraftBody  string
		msgCreatedAt   time.Time // when first draft/stream msg was sent
		draftFailCount int      // consecutive draft failures in this turn
		editFailCount  int      // consecutive edit failures in this turn
	)
	const (
		maxDraftFailures = 3
		maxEditFailures  = 3
		freshFinalAfter  = 30 * time.Second
		streamDebounce   = 50 * time.Millisecond
	)
	var procErr error
	replyDelivered := false

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
		streamDone = true

		bufMu.Lock()
		body := buf.String()
		tr := truncated
		bufMu.Unlock()
		body = strings.TrimSpace(body)
		if body == "" {
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
			n = a.finalizeDraft(chatID, draftID, body)
			log.Printf("chat=%d draftID=%d: finalize %d part(s) total=%d runes", chatID, draftID, n, utf8.RuneCountInString(body))
		} else {
			if useFreshFinal && streamMsgID > 0 {
				log.Printf("chat=%d: fresh final (stale preview >%ds), sending new message", chatID, int(freshFinalAfter.Seconds()))
			}
			n = a.sendTextParts(chatID, body, nil)
			if n == 0 {
				log.Printf("chat=%d: finalize send failed (0 parts)", chatID)
			}
		}
		if n > 0 {
			replyDelivered = true
		}
	}

	pushDraft := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			return
		}
		bufMu.Lock()
		body := strings.TrimSpace(buf.String())
		bufMu.Unlock()
		if body == "" {
			return
		}
		preview := telegramPreviewTail(body, telegramMaxMessageRunes)
		if useDraft {
			if preview == lastDraftBody {
				return
			}
			if a.sendDraft(chatID, draftID, preview) {
				draftFailCount = 0
				lastDraftBody = preview
				if msgCreatedAt.IsZero() {
					msgCreatedAt = time.Now()
				}
				return
			}
			// Draft failed
			draftFailCount++
			if draftFailCount >= maxDraftFailures {
				log.Printf("chat=%d: disabling draft stream after %d failures", chatID, draftFailCount)
				useDraft = false
			}
			// Fall through to edit path for this frame
		}
		if streamMsgID == 0 {
			previewHTML := formatForTelegram(preview)
			msg := tgbotapi.NewMessage(chatID, previewHTML)
			msg.ParseMode = "HTML"
			sent, err := a.bot.Send(msg)
			if err != nil {
				log.Printf("chat=%d: stream send failed: %v", chatID, err)
				editFailCount++
				return
			}
			editFailCount = 0
			streamMsgID = sent.MessageID
			lastDraftBody = preview
			if msgCreatedAt.IsZero() {
				msgCreatedAt = time.Now()
			}
			return
		}
		if preview == lastDraftBody {
			return
		}
		previewHTML := formatForTelegram(preview)
		edit := tgbotapi.NewEditMessageText(chatID, streamMsgID, previewHTML)
		edit.ParseMode = "HTML"
		if _, err := a.bot.Send(edit); err == nil {
			editFailCount = 0
			lastDraftBody = preview
			return
		}
		editFailCount++
		if editFailCount >= maxEditFailures {
			log.Printf("chat=%d: edit flood-silenced after %d failures", chatID, editFailCount)
		}
	}

	signalFlush := func() {
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

	go func() {
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
		)
		close(finished)
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
			pushDraft()
			draftMu.Lock()
			bufMu.Lock()
			body := strings.TrimSpace(buf.String())
			buf.Reset()
			truncated = false
			bufMu.Unlock()
			if body != "" {
				if useDraft {
					a.finalizeDraft(chatID, draftID, body)
				} else {
					a.sendTextParts(chatID, body, nil)
				}
				replyDelivered = true
			}
			// Reset draft/send state for next segment
			draftID = a.nextDraftID()
			useDraft = true
			lastDraftBody = ""
			streamMsgID = 0
			msgCreatedAt = time.Now()
			draftMu.Unlock()
		}
		for {
			select {
			case <-pushWake:
				stopDebounce()
				debounce.Reset(streamDebounce)
			case <-debounce.C:
				pushDraft()
			case <-newSegment:
				newSegmentHandler()
			case <-flushNow:
				flushAndEnd()
			case <-finished:
				flushAndEnd()
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
		streamDone = true
		draftMu.Unlock()
		if replyDelivered && errors.Is(procErr, context.Canceled) {
			log.Printf("chat=%d prompt=%q: canceled after reply delivered", chatID, logPreview(prompt, 80))
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

	if empty && streamMsgID == 0 && useDraft {
		// sendMessage dismisses the native draft preview; never clearDraft(empty) here.
		if !replyDelivered {
			a.reply(chatID, "（模型没有返回文字）")
		}
	}
	bufMu.Lock()
	finalBody := strings.TrimSpace(buf.String())
	bufMu.Unlock()
	log.Printf("chat=%d prompt=%q stream=draft draftID=%d finalLen=%d runes=%d parts-cap=%d msgID=%d",
		chatID, logPreview(prompt, 80), draftID, len(finalBody), utf8.RuneCountInString(finalBody), telegramMaxMessageRunes, streamMsgID)
}

// nextDraftID returns a unique Telegram draft_id (int32-safe, no second-level collisions).
func (a *App) nextDraftID() int64 {
	seq := atomic.AddUint64(&a.draftSeq, 1)
	// Low 9 digits from unix seconds + 4-digit sequence within the same second.
	return int64(time.Now().Unix()%1_000_000_000)*10000 + int64(seq%10000)
}

// finalizeDraft ends a native-draft turn: last draft frame, then one or more sendMessage parts.
// Never send empty sendMessageDraft here; that creates a ghost bubble revoked seconds later.
func (a *App) finalizeDraft(chatID int64, draftID int64, text string) int {
	parts := splitTelegramText(text, telegramMaxMessageRunes)
	if len(parts) == 0 {
		return 0
	}
	_ = a.sendDraft(chatID, draftID, parts[0])
	return a.sendTextParts(chatID, text, nil)
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
		"parse_mode": "HTML",
	})
	if err != nil {
		log.Printf("sendMessageDraft failed (fallback to edit): %v", err)
		return false
	}
	return true
}

func (a *App) healthHandler(m *tgbotapi.Message) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	lines := []string{fmt.Sprintf("模式: %s", a.cfg.Mode)}
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
		a.reply(m.Chat.ID, "🔄 服务重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	var newMode string
	switch arg {
	case "chat", "":
		newMode = ModeChat
	case "tool":
		newMode = ModeTool
	default:
		a.reply(m.Chat.ID, "可用模式：`chat`（纯对话，工具关闭）或 `tool`（全能编程）\n用法：/mode chat 或 /mode tool")
		return
	}
	if a.cfg.Mode == newMode {
		a.reply(m.Chat.ID, fmt.Sprintf("当前已经是 %s 模式", newMode))
		return
	}
	// Stop existing serve, switch mode, rewrite toml, restart.
	a.stopServe(m.Chat.ID)
	a.cfg.Mode = newMode
	_ = a.ensureChatWorkdir()
	if err := a.startServe(m.Chat.ID); err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("切换模式失败: %v", err))
		return
	}
	modeLabel := "纯对话（工具关闭）"
	if newMode == ModeTool {
		modeLabel = "全能编程（工具可用）"
	}
	a.reply(m.Chat.ID, fmt.Sprintf("已切换到 %s 模式\n%s", newMode, modeLabel))
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


