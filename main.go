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
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var version = "dev" // set by ldflags at build time

// Telegram shows "typing…" for ~5s per sendChatAction; refresh before it expires.
const typingRefreshInterval = 4 * time.Second

// Strip ANSI / TUI art from reasonix output. Reasonix in `run` mode emits
// progress blocks like "\x1b[2m  ▎ thinking [0m" which render as garbage
// in Telegram. We strip CSI sequences (\x1b[...m) and OSC sequences, plus
// the bracket-art lines reasonix draws between blocks.
//
// We keep the textual content; if the user actually wants TUI fidelity,
// they should run reasonix locally, not through a chat bot.
var ansiCSI = regexp.MustCompile(`\x1b\[[0-9;:?]*[a-zA-Z]`)
var ansiOSC = regexp.MustCompile(`\x1b\][^\x07]*\x07`)
var ansiBare = regexp.MustCompile(`\x1b[@-_]`) // ESC followed by a single byte (e.g. ESC c, ESC =)
// ST-terminated OSC sequences: ESC ] ... ST (ST = ESC \)
var ansiOSCST = regexp.MustCompile(`\x1b\][^\x1b]*\x1b\\`)

func stripANSI(s string) string {
	s = ansiOSC.ReplaceAllString(s, "")
	s = ansiOSCST.ReplaceAllString(s, "")
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
	// "  · status: some status message from reasonix"
	// Catch any `· <text>:` status line that reasonix prints
	reStatusDot = regexp.MustCompile(`^\s*·\s+\S+:`)
	// "  ▎ thinking" / "  ▎ ..."   (TUI bullet bar)
	reThinkingBar = regexp.MustCompile(`^\s*[▎▌▍▏┃│]\s*(thinking|reasoning|done|working|executing|reading|writing|searching)\b`)
	// "hook <name>: ..." — tool hook status lines
	reHookLine = regexp.MustCompile(`^\s*hook\s`)
	// "[ctx-x]" context reference lines
	reCtxLine = regexp.MustCompile(`^\s*\[ctx`)
	// "unknown ref"/"unknown tool" lines
	reUnknownRef = regexp.MustCompile(`unknown (ref|tool)`)
	// Timestamp log lines: 2026/06/21 08:25:12 INFO|WARN|ERROR|DEBUG ...
	reLogLine = regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} (INFO|WARN|ERROR|DEBUG) `)
)

// isReasonixNoise returns true if the line should be dropped before display.
func isReasonixNoise(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false // 保留空行作为段落分隔
	}
	if strings.HasPrefix(trimmed, "❌") || strings.HasPrefix(trimmed, "✅") || strings.HasPrefix(trimmed, "ℹ️") || strings.HasPrefix(trimmed, "hook ") || strings.HasPrefix(trimmed, "[ctx]") || strings.HasPrefix(trimmed, "exit status") || strings.HasPrefix(trimmed, "command exited") || strings.HasPrefix(trimmed, "remembered") || strings.HasPrefix(trimmed, "unknown ref") || strings.HasPrefix(trimmed, "unknown tool") || strings.Contains(trimmed, "unknown tool") {
		return true
	}
	if reThinkingBar.MatchString(trimmed) {
		return true
	}
	if reTokenStats.MatchString(trimmed) {
		return true
	}
	if reStatusDot.MatchString(trimmed) {
		return true
	}
	if reHookLine.MatchString(trimmed) {
		return true
	}
	if reCtxLine.MatchString(trimmed) {
		return true
	}
	if reUnknownRef.MatchString(trimmed) {
		return true
	}
	// 新增：-> 开头的 tool 调用重定向行
	if strings.HasPrefix(trimmed, "->") {
		return true
	}
	// 新增：⊘ 开头的 tool 错误行
	if strings.HasPrefix(trimmed, "⊘") {
		return true
	}
	// 新增：时间戳日志行 (2026/06/21 08:25:12 INFO/WARN/ERROR)
	if reLogLine.MatchString(trimmed) {
		return true
	}
	return false
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

// reasonixDefaultModel is the default_model from reasonix config.toml,
// populated from reasonix doctor --json at startup. Used instead of env MODEL.
var reasonixDefaultModel string

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
			Model       string   `json:"model"`
			Models      []string `json:"models"`
			IsDefault   bool     `json:"is_default"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		log.Printf("loadModels: parse doctor json: %v", err)
		return
	}
	// Collect models from all providers, marking the default.
	reasonixDefaultModel = ""
	availableModels = availableModels[:0]
	for _, p := range doc.Providers {
		for _, m := range p.Models {
			id := p.Name + "/" + m
			display := m
			if doc.Config.DefaultModel != "" && doc.Config.DefaultModel == id {
				reasonixDefaultModel = id
			} else if reasonixDefaultModel == "" && p.Model != "" && m == p.Model {
				reasonixDefaultModel = id
			}
			availableModels = append(availableModels, struct {
				ID   string
				Name string
			}{id, display})
		}
	}
	// Fallback: use first model of first provider.
	if reasonixDefaultModel == "" && len(availableModels) > 0 {
		reasonixDefaultModel = availableModels[0].ID
	}
	log.Printf("loadModels: loaded %d models from reasonix, default=%s", len(availableModels), reasonixDefaultModel)
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
	MaxDuration    int     // MAX_DURATION_MIN, default 120
	StateDir string // STATE_DIR, default /var/lib/reasonix-telegram
	Mode     string // MODE: "chat" (default, tools locked) or "tool" (full agent access)
	DeepSeekKey string // read from /etc/reasonix-api.env, never in os.Environ
	NotificationMode string // NOTIFICATION_MODE: "important" (default) or "all"
	WorkDir string // WORK_DIR: reasonix serve working directory (tool workspace); default = /root
	secrets []string // collected at startup for log redaction
}

func loadEnvFile(path, key string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	perm := fi.Mode().Perm()
	if perm&0077 != 0 {
		return "", fmt.Errorf("api key file %s has overly permissive permissions %04o, must be 0600 or stricter", path, perm)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
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
			return val, nil
		}
	}
	return "", nil
}

func loadConfig() (Config, error) {
	mode := getenv("MODE", ModeChat)
	if mode != ModeChat && mode != ModeTool {
		mode = ModeChat
	}
	deepSeekKey, err := loadEnvFile("/etc/reasonix-api.env", "DEEPSEEK_API_KEY")
	if err != nil {
		return Config{}, err
	}
	c := Config{
		BotToken:       os.Getenv("TG_BOT_TOKEN"),
		ReasonixBin:    getenv("REASONIX_BIN", "reasonix"),
		MaxOutputBytes: atoi(getenv("MAX_OUTPUT_BYTES", "524288")),
		MaxDuration:    atoi(getenv("MAX_DURATION_MIN", "120")),
		StateDir: getenv("STATE_DIR", defaultStateDir),
		Mode:           mode,
		DeepSeekKey:    deepSeekKey,
		NotificationMode: getenv("NOTIFICATION_MODE", "important"),
		WorkDir:        os.Getenv("WORK_DIR"),
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
	// Collect secrets for log redaction (env will be unset after startup).
	if c.BotToken != "" {
		c.secrets = append(c.secrets, c.BotToken)
	}
	if c.DeepSeekKey != "" {
		c.secrets = append(c.secrets, c.DeepSeekKey)
	}
	if cf := os.Getenv("CF_TOKEN"); cf != "" {
		c.secrets = append(c.secrets, cf)
	}
	return c, nil
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
	hmacKey          []byte    // 32-byte HMAC-SHA256 key for callback signing
	// Cumulative session totals (accumulated across turns).
	cumPrompt     int
	cumCompletion int
	cumTotal      int
	cumCost       float64
	cumCurrency   string
	liveDraftID   int64 // open sendMessageDraft on Telegram (session-level for pre-empt cleanup)
	serveRestartCount int       // consecutive health-check restart failures
	serveLastRestart  time.Time // last restart attempt time
	encryptDone       chan struct{} // closed when the encrypt-from-plain goroutine finishes
}

type runningTask struct {
	cancel     context.CancelFunc
	stopTyping context.CancelFunc // nil if typing not started yet
}

type App struct {
	cfg         Config
	bridge      PlatformBridge
	bot         *tgbotapi.BotAPI
	cronManager *CronManager
	state        *stateStore
	handlerSem   chan struct{} // max concurrent handleMessage/handleCallbackQuery
	sessMu       sync.Mutex
	sess       map[int64]*session
	restartMu      sync.Mutex
	restarting     bool
	restartStarted time.Time
	msgWg          sync.WaitGroup // tracks in-flight handleMessage goroutines
	draftSeq       uint64 // per-process draft_id sequence (avoids same-second collisions)
	clarifySeq     uint64 // monotonic counter for clarify IDs
	mode                atomic.Value // string: ModeChat or ModeTool
	restartingInProgress atomic.Bool
	sentTextCache  sync.Map     // message_id → sent text (for reply/quote extraction)
	sentTextCachePath string       // path to sent_text_cache.json on disk
	sentTextCacheMu   sync.Mutex   // guards saveSentTextCache disk write
	sentTextCacheTimer  *time.Timer   // debounce timer for saveSentTextCache
	sentTextCacheTimerMu sync.Mutex   // guards timer
	cacheMu          sync.Mutex   // guards cleanupCacheDir
	stickerMu         sync.Mutex   // guards stickerCache load/save/handle

	mediaGroupsMu sync.Mutex
	mediaGroups   map[int64]map[string]*mediaGroupBatch // chatID → mediaGroupID → batch

	rateLimits sync.Map // map[int64]time.Time — per-chat last message time

	noticeMu   sync.Mutex
	lastNotice map[string]time.Time // "chatID|noticeText" → last seen time

	sentCache   map[string]time.Time // content hash → send time (dedup guard)
	sentCacheMu sync.Mutex

	healthCheckStop chan struct{} // closed on shutdown to stop health check goroutine
}

// redactSecrets returns s with known secrets replaced by "***".
// Secrets are collected at startup before env is stripped.
func redactSecrets(s string, secrets []string) string {
	for _, v := range secrets {
		if v != "" && v != s {
			s = strings.ReplaceAll(s, v, "***")
		}
	}
	return s
}

func (a *App) getMode() string {
	if v := a.mode.Load(); v != nil {
		if s, ok := v.(string); ok {
			return s
		}
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
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Strip API keys from process environment so /proc/<pid>/environ doesn't leak them.
	// StartServe passes DEEPSEEK_API_KEY to child processes via a temp file + DEEPSEEK_API_KEY_FILE.
	os.Unsetenv("DEEPSEEK_API_KEY")
	os.Unsetenv("TG_BOT_TOKEN")
	os.Unsetenv("CF_TOKEN")
	if cfg.BotToken == "" {
		log.Fatal("TG_BOT_TOKEN is required")
	}
	if _, err := exec.LookPath(cfg.ReasonixBin); err != nil {
		log.Fatalf("reasonix binary not found on PATH: %s (%v)", cfg.ReasonixBin, err)
	}
	loadModelsFromReasonix(cfg.ReasonixBin)

	bridge, err := NewTelegramBridge(&cfg)
	if err != nil {
		log.Fatalf("telegram auth failed: %v", redactSecrets(err.Error(), cfg.secrets))
	}
	bot := bridge.GetBot()

	if err := bridge.RegisterCommands(); err != nil {
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
		bridge:      bridge,
		bot:         bot,
		state:        st,
		handlerSem:   make(chan struct{}, 100),
		sess:         map[int64]*session{},
		mediaGroups: map[int64]map[string]*mediaGroupBatch{},
		sentCache:   make(map[string]time.Time),
		healthCheckStop: make(chan struct{}),
	}
	app.setMode(cfg.Mode)
	if err := app.ensureUserRulesLinked(); err != nil {
		log.Fatalf("link user rules: %v", err)
	}
	log.Printf("mode=%s workdir=%s", app.cfg.Mode, app.chatWorkdir())
	log.Printf("telegram stream: sendMessageDraft + sendMessage finalize (TelePi/Hermes pattern)")

	// Load persisted sent-text cache for reply/quote extraction across restarts.
	app.sentTextCachePath = filepath.Join(st.dir, "sent_text_cache.json.enc")
	app.loadSentTextCache()

	app.startRestartWatchdog()
	app.cleanupStaleServesOnStartup()
	app.restorePersistedSessions()
	app.startServeHealthCheck()
	app.notifyBridgeRestarted()
	app.initCron()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// Periodic cleanup of seenMsgs (message_id dedup) — remove entries older than 10 min.
	go func() {
		ticker := time.NewTicker(10 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				now := time.Now().Unix()
				seenMsgs.Range(func(key, value any) bool {
					if ts, ok := value.(int64); ok && now-ts > 600 {
						seenMsgs.Delete(key)
					}
					return true
				})
			case <-ctx.Done():
				return
			}
		}
	}()

	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bridge.GetUpdatesChan(u)

	for {
		select {
		case <-ctx.Done():
			if app.restartingInProgress.Load() {
				log.Printf("Restart in progress, skipping graceful shutdown cleanup")
				os.Exit(0)
			}
			log.Printf("shutdown signal: flushing reasonix sessions…")
			close(app.healthCheckStop)
			app.cancelAllTasks()
			app.msgWg.Wait()
			app.waitTasksDone(5 * time.Second)
			app.stopAllServes()
			// Stop sentTextCache debounce timer and flush remaining data.
			app.sentTextCacheTimerMu.Lock()
			if app.sentTextCacheTimer != nil {
				app.sentTextCacheTimer.Stop()
				app.sentTextCacheTimer = nil
			}
			app.sentTextCacheTimerMu.Unlock()
			app.saveSentTextCache()
			if app.cronManager != nil {
				app.cronManager.cron.Stop()
				// Wait for running cron jobs to finish (max 30s timeout).
				done := make(chan struct{})
				go func() {
					app.cronManager.runningJobs.Wait()
					close(done)
				}()
				select {
				case <-done:
					log.Println("cron: all running jobs completed")
				case <-time.After(30 * time.Second):
					log.Println("cron: timed out waiting for running jobs after 30s")
				}
				if app.cronManager.watcher != nil {
					app.cronManager.watcher.Close()
				}
			}
			return
		case upd, ok := <-updates:
			if !ok {
				app.msgWg.Wait()
				return
			}
			if upd.Message != nil {
				app.msgWg.Add(1)
				app.handlerSem <- struct{}{}
				go func() {
					defer func() { <-app.handlerSem }()
					app.handleMessage(upd.Message)
				}()
			}
			if upd.CallbackQuery != nil {
				app.msgWg.Add(1)
				app.handlerSem <- struct{}{}
				go func() {
					defer func() { <-app.handlerSem }()
					app.handleCallbackQuery(upd.CallbackQuery)
				}()
			}
		}
	}
}
