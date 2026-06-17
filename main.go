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
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
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
	if strings.HasPrefix(trimmed, "❌") || strings.HasPrefix(trimmed, "✅") || strings.HasPrefix(trimmed, "ℹ️") || strings.HasPrefix(trimmed, "hook ") || strings.HasPrefix(trimmed, "[ctx]") || strings.HasPrefix(trimmed, "exit status") || strings.HasPrefix(trimmed, "command exited") || strings.HasPrefix(trimmed, "remembered") || strings.HasPrefix(trimmed, "unknown ref") {
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
	DeepSeekKey string // read from /etc/reasonix-api.env, never in os.Environ
	NotificationMode string // NOTIFICATION_MODE: "important" (default) or "all"
}

func loadEnvFile(path, key string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, key+"=") {
			val := strings.TrimPrefix(line, key+"=")
			val = strings.TrimSpace(val)
			// Strip surrounding quotes (single or double).
			if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
			return val
		}
	}
	return ""
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
		DeepSeekKey: loadEnvFile("/etc/reasonix-api.env", "DEEPSEEK_API_KEY"),
		NotificationMode: getenv("NOTIFICATION_MODE", "important"),
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
	sentTextCache  sync.Map     // message_id → sent text (for reply/quote extraction)

	mediaGroupsMu sync.Mutex
	mediaGroups   map[int64]map[string]*mediaGroupBatch // chatID → mediaGroupID → batch
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

// redactSecrets returns s with known secrets replaced by "***".
// Apply to any log string that might contain tokens or API keys.
func redactSecrets(s string, extraSecrets ...string) string {
	secrets := extraSecrets
	for _, env := range []string{"TG_BOT_TOKEN", "DEEPSEEK_API_KEY", "CF_TOKEN"} {
		if v := os.Getenv(env); v != "" && v != s {
			secrets = append(secrets, v)
		}
	}
	for _, v := range secrets {
		if v != "" && v != s {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

func logRedact(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	log.Print(redactSecrets(msg))
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
		return "编程模式"
	}
	return "聊天模式"
}

func (a *App) modeLabel() string {
	return modeLabelFor(a.getMode())
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	cfg := loadConfig()

	// Strip API key from process environment so /proc/<pid>/environ doesn't leak it.
	// reasonixEnv() passes it explicitly to child processes from cfg.DeepSeekKey.
	os.Unsetenv("DEEPSEEK_API_KEY")
	os.Unsetenv("TG_BOT_TOKEN")
	if cfg.BotToken == "" {
		log.Fatal("TG_BOT_TOKEN is required")
	}
	if _, err := exec.LookPath(cfg.ReasonixBin); err != nil {
		log.Fatalf("reasonix binary not found on PATH: %s (%v)", cfg.ReasonixBin, err)
	}
	loadModelsFromReasonix(cfg.ReasonixBin)

	bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
	if err != nil {
		log.Fatalf("telegram auth failed: %v", redactSecrets(err.Error(), cfg.BotToken))
	}
	bot.Debug = false
	// Wrap HTTP client to redact bot token from error logs
	// and enable fallback transport for GFW-broken networks.
	innerTransport := &tokenRedactingTransport{
		inner: http.DefaultTransport,
		token: cfg.BotToken,
	}
	bot.Client = &http.Client{
		Transport: NewTelegramFallbackTransport(innerTransport, os.Getenv("TELEGRAM_FALLBACK_IPS")),
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
	app := &App{
		cfg:         cfg,
		bot:         bot,
		state:       st,
		sess:        map[int64]*session{},
		mediaGroups: map[int64]map[string]*mediaGroupBatch{},
	}
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
