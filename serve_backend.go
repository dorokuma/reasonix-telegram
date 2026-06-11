package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var portNext int // next port offset, guarded by a.state itself during init

func portForChat(chatID int64) int {
	const base = 18780
	const span = 8000
	p := base + (portNext % span)
	portNext++
	return p
}

func serveAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func serveBaseURL(port int) string {
	return fmt.Sprintf("http://%s", serveAddr(port))
}

// wireUsage mirrors usage stats from the reasonix serve SSE stream.
type wireUsage struct {
	PromptTokens     int     `json:"promptTokens"`
	CompletionTokens int     `json:"completionTokens"`
	TotalTokens      int     `json:"totalTokens"`
	CacheHitTokens   int     `json:"cacheHitTokens,omitempty"`
	CacheMissTokens  int     `json:"cacheMissTokens,omitempty"`
	Cost             float64 `json:"cost,omitempty"`
	Currency         string  `json:"currency,omitempty"`
	CostUSD          float64 `json:"costUsd,omitempty"`
	// Session-cumulative cache stats (sent by serve, per-turn fields dropped).
	SessionCacheHitTokens  int `json:"sessionCacheHitTokens,omitempty"`
	SessionCacheMissTokens int `json:"sessionCacheMissTokens,omitempty"`
}

// wireEvent mirrors reasonix serve SSE JSON (internal/serve/wire.go).
type wireEvent struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Reasoning string `json:"reasoning,omitempty"`
	Err       string `json:"err,omitempty"`
	Tool      *struct {
		Name    string `json:"name"`
		Args    string `json:"args,omitempty"`
		Output  string `json:"output,omitempty"`
		Err     string `json:"err,omitempty"`
		Partial bool   `json:"partial,omitempty"`
	} `json:"tool,omitempty"`
	Approval *struct {
		ID   string `json:"id"`
		Tool string `json:"tool"`
	} `json:"approval,omitempty"`
	Ask   *wireAsk    `json:"ask,omitempty"`
	Usage *wireUsage  `json:"usage,omitempty"`
}

// wireAsk mirrors reasonix serve's ask_request event (internal/serve/wire.go).
type wireAsk struct {
	ID        string            `json:"id"`
	Questions []wireAskQuestion `json:"questions"`
}

type wireAskQuestion struct {
	ID      string          `json:"id"`
	Prompt  string          `json:"prompt,omitempty"`
	Multi   bool            `json:"multi,omitempty"`
	Options []wireAskOption `json:"options"`
}

type wireAskOption struct {
	Label string `json:"label"`
}

// askQuestionData is a simplified question passed to the onAskRequest callback.
type askQuestionData struct {
	ID      string
	Options []string
	Text    string // question text accumulated from model output
}

type turnResult struct {
	err error
}

func (a *App) reasonixEnv() []string {
	// Build a minimal environment for child processes — never inherit all
	// parent env vars (which may contain API keys, tokens, secrets).
	var env []string

	// Pass through safe variables that reasonix may need.
	safePrefixes := []string{
		"HOME=", "USER=", "LOGNAME=", "SHELL=",
		"PATH=", "LANG=", "LC_", "LANGUAGE=", "TZ=",
		"HTTP_PROXY=", "HTTPS_PROXY=", "NO_PROXY=",
		"http_proxy=", "https_proxy=", "no_proxy=",
		"EDITOR=", "VISUAL=", "PAGER=",
		"XDG_CACHE_HOME=", "XDG_CONFIG_HOME=", "XDG_DATA_HOME=", "XDG_STATE_HOME=",
	}
	for _, e := range os.Environ() {
		for _, p := range safePrefixes {
			if strings.HasPrefix(e, p) {
				env = append(env, e)
				break
			}
		}
	}

	// Ensure PATH includes common bin locations.
	hasUsrLocal := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") && strings.Contains(e, "/usr/local/bin") {
			hasUsrLocal = true
			break
		}
	}
	if !hasUsrLocal {
		for i, e := range env {
			if strings.HasPrefix(e, "PATH=") {
				env[i] = e + ":/usr/local/bin"
				break
			}
		}
	}

	// Only DEEPSEEK_API_KEY is intentionally forwarded.
	if k := os.Getenv("DEEPSEEK_API_KEY"); k != "" {
		env = append(env, "DEEPSEEK_API_KEY="+k)
	}

	// systemd ProtectHome=read-only blocks /root/.cache; keep caches under StateDir.
	cacheBase := filepath.Join(a.cfg.StateDir, "cache")
	env = append(env,
		"REASONIX_CACHE_DIR="+cacheBase,
		"XDG_CACHE_HOME="+cacheBase,
	)

	if a.getMode() == ModeChat {
		env = append(env, "NO_COLOR=1", "FORCE_COLOR=0", "CI=1", "TERM=dumb")
	}
	return env
}

func (a *App) serveRunning(s *session) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	cmd := s.serveCmd
	port := s.servePort
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil && cmd.ProcessState == nil {
		return true
	}
	if port == 0 {
		return false
	}
	// Adopt an already-listening serve (e.g. after an out-of-band restart) so the
	// bridge does not try to bind the same port again.
	return a.waitServeReady(port, 2*time.Second) == nil
}

func (a *App) stopServe(chatID int64) {
	s := a.getOrCreateSession(chatID)
	a.stopSessionServe(s, chatID)
}

// stopSessionServe stops the serve command for an already-looked-up session.
// Used by stopServe and stopAllServes (which holds sessMu). Does NOT lock sessMu.
func (a *App) stopSessionServe(s *session, chatID int64) {
	s.mu.Lock()
	cmd := s.serveCmd
	s.serveCmd = nil
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		// The bridge already sent /cancel via cancelAllTasks + waitTasksDone.
		// Give the serve process 3s to flush its session JSONL before SIGTERM,
		// so the last few messages survive a restart.
		log.Printf("chat=%d: stopping serve (pid %d), 3s grace for session flush…", chatID, cmd.Process.Pid)
		time.Sleep(3 * time.Second)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		waitDone := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
			log.Printf("chat=%d: serve exited cleanly", chatID)
		case <-time.After(10 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitDone
		}
	}
}

func (a *App) startServe(chatID int64) error {
	if err := a.ensureChatWorkdir(); err != nil {
		return err
	}
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.serveCmd != nil && s.serveCmd.Process != nil && s.serveCmd.ProcessState == nil {
		s.mu.Unlock()
		return nil
	}
	wd := a.chatWorkdir()
	s.workdir = wd
	sessionPath := s.sessionPath
	port := s.servePort
	model := s.model // per-session model override
	s.mu.Unlock()

	if sessionPath == "" {
		sessionPath = a.state.sessionPathForChat(chatID)
		s.mu.Lock()
		s.sessionPath = sessionPath
		s.servePort = port
		s.mu.Unlock()
	}

	msgs, users, _ := sessionStats(sessionPath)
	if users > 0 {
		log.Printf("chat=%d: resume session %s (%d messages, %d user turns)", chatID, sessionPath, msgs, users)
	} else {
		// No pre-created empty JSONL — an empty --resume file used to replace the
		// boot-time system prompt and drop global REASONIX.md from the model context.
		_ = os.Remove(sessionPath)
		log.Printf("chat=%d: new session at %s", chatID, sessionPath)
	}

	args := []string{"serve", "--addr", serveAddr(port)}
	// Use per-session model if set, otherwise fall back to global config.
	if model == "" {
		model = a.cfg.Model
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	// Always --resume so auto-save stays on sessionPath (not ~/.config/reasonix/sessions).
	// Reasonix Resume now re-applies the boot system prompt even when the file is empty.
	args = append(args, "--resume", sessionPath)

	if err := a.waitServeReady(port, 2*time.Second); err == nil {
		log.Printf("chat=%d: adopting existing reasonix serve on %s", chatID, serveAddr(port))
		return nil
	}

	cmd := exec.Command(a.cfg.ReasonixBin, args...)
	cmd.Dir = wd
	cmd.Env = a.reasonixEnv()
	// Go wires nil Stderr to /dev/null; forward serve diagnostics (RTK/CTX hit/miss) to journal.
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start reasonix serve: %w", err)
	}

	s.mu.Lock()
	s.serveCmd = cmd
	s.sessionPath = sessionPath
	s.servePort = port
	s.mu.Unlock()

	go func() {
		err := cmd.Wait()
		s.mu.Lock()
		if s.serveCmd == cmd {
			s.serveCmd = nil
		}
		s.mu.Unlock()
		if err != nil {
			log.Printf("chat=%d: reasonix serve exited: %v", chatID, err)
		}
	}()

	if err := a.waitServeReady(port, 45*time.Second); err != nil {
		a.stopServe(chatID)
		return err
	}
	// mode lockdown is handled by reasonix.toml
	log.Printf("chat=%d: serve cwd=%s mode=%s", chatID, wd, a.getMode())
	if err := a.state.upsert(chatRecord{
		ChatID: chatID, Workdir: wd, SessionPath: sessionPath, Port: port,
	}); err != nil {
		log.Printf("chat=%d: persist state failed: %v", chatID, err)
	}
	log.Printf("chat=%d: reasonix serve ready on %s session=%s", chatID, serveAddr(port), sessionPath)
	return nil
}

func (a *App) ensureServe(chatID int64) error {
	if a.serveRunning(a.getOrCreateSession(chatID)) {
		return nil
	}
	return a.startServe(chatID)
}

// startServeHealthCheck periodically checks all serve processes are alive.
// Runs every 60s; restarts any that have died.
func (a *App) startServeHealthCheck() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			a.sessMu.Lock()
			chatIDs := make([]int64, 0, len(a.sess))
			for chatID, s := range a.sess {
				s.mu.Lock()
				cmd := s.serveCmd
				port := s.servePort
				s.mu.Unlock()
				// Check if process is dead but port not listening
				alive := cmd != nil && cmd.Process != nil && cmd.ProcessState == nil
				listening := port > 0 && a.waitServeReady(port, 2*time.Second) == nil
				if !alive && !listening {
					chatIDs = append(chatIDs, chatID)
				}
			}
			a.sessMu.Unlock()
			for _, chatID := range chatIDs {
				log.Printf("health: chat=%d serve process dead, restarting", chatID)
				if err := a.startServe(chatID); err != nil {
					log.Printf("health: chat=%d restart failed: %v", chatID, err)
				}
			}
		}
	}()
}

func (a *App) restorePersistedSessions() {
	a.state.cleanupOrphanSessionArtifacts()
	records, err := a.state.load()
	if err != nil {
		log.Printf("warning: load persisted state: %v", err)
		return
	}
	known := map[int64]struct{}{}
	for _, rec := range records {
		known[rec.ChatID] = struct{}{}
	}
	for _, chatID := range a.state.chatIDsWithSessionJSONL() {
		if _, ok := known[chatID]; ok {
			continue
		}
		records = append(records, chatRecord{
			ChatID:      chatID,
			SessionPath: a.state.sessionPathForChat(chatID),
			Port:        portForChat(chatID),
		})
		log.Printf("startup: recovered orphan session jsonl for chat=%d", chatID)
	}
	for _, rec := range records {
		s := a.getOrCreateSession(rec.ChatID)
		s.mu.Lock()
		s.workdir = a.chatWorkdir()
		s.sessionPath = rec.SessionPath
		if s.sessionPath == "" {
			s.sessionPath = a.state.sessionPathForChat(rec.ChatID)
		}
		s.servePort = rec.Port
		if s.servePort == 0 {
			s.servePort = portForChat(rec.ChatID)
		}
		s.mu.Unlock()
		go func(chatID int64) {
			if err := a.startServe(chatID); err != nil {
				log.Printf("chat=%d: restore serve failed (will retry on next message): %v", chatID, err)
			}
		}(rec.ChatID)
	}
	if len(records) > 0 {
		log.Printf("restoring %d persisted reasonix session(s)", len(records))
	}
}

func (a *App) waitServeReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := serveBaseURL(port) + "/status"
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("reasonix serve not ready at %s", url)
}

func (a *App) postJSON(port int, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, serveBaseURL(port)+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

// runServeTurn submits a prompt to the long-lived reasonix serve process and
// streams SSE events until turn_done. The conversation history stays in the
// same Reasonix session file across Telegram messages and bridge restarts.
func (a *App) runServeTurn(ctx context.Context, chatID int64, prompt string, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string) int, onAskRequest func(askID string, questions []askQuestionData), onApprovalRequest func(approvalID string, toolName string), onUsage func(wireUsage)) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()

	eventsDone := make(chan turnResult, 1)
	go func() {
		eventsDone <- a.consumeServeEvents(ctx, chatID, port, onChunk, onComplete, onToolDispatch, onCommentary, onAskRequest, onApprovalRequest, onUsage)
	}()

	if err := a.postJSON(port, "/submit", map[string]string{"input": prompt}); err != nil {
		cancel()
		return fmt.Errorf("submit: %w", err)
	}

	select {
	case tr := <-eventsDone:
		return tr.err
	case <-ctx.Done():
		_ = a.postJSON(port, "/cancel", map[string]any{})
		select {
		case tr := <-eventsDone:
			return tr.err
		case <-time.After(8 * time.Second):
			return ctx.Err()
		}
	}
}

func (a *App) consumeServeEvents(ctx context.Context, chatID int64, port int, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string) int, onAskRequest func(askID string, questions []askQuestionData), onApprovalRequest func(approvalID string, toolName string), onUsage func(wireUsage)) turnResult {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, serveBaseURL(port)+"/events", nil)
	if err != nil {
		return turnResult{err: err}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return turnResult{err: err}
	}
	defer resp.Body.Close()

	var turnErr error
	var gotTextDelta bool
	var sawToolDispatch bool
	var reasoningBuf strings.Builder
	var cancelOnce sync.Once
	var lastToolMsgID int
	var lastToolText string // raw text of last tool dispatch (for appending result)
	var lastToolName string // last dispatched tool name (for consolidation)
	var toolCount int      // consecutive same-tool calls
	var bufferingAsk bool // true while accumulating question text for ask tool
	var askTextBuffer strings.Builder

	// SSE idle watchdog: close body if no data for 5 min (Hermes-inspired).
	var lastActivityUnix int64 = time.Now().Unix()
	watchdogCtx, watchdogCancel := context.WithCancel(ctx)
	defer watchdogCancel()
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Now().Unix() - atomic.LoadInt64(&lastActivityUnix)
				if elapsed > 300 {
					log.Printf("port=%d: SSE idle for %ds, closing stream", port, elapsed)
					resp.Body.Close()
					return
				}
			case <-watchdogCtx.Done():
				return
			}
		}
	}()

	// Hermes handles reasoning-only turns in the agent loop (prefill continuation
	// + retries). Reasonix accepts them as a successful final answer, so the bridge
	// must recover visible text from accumulated reasoning when no text deltas arrive.
	flushReasoningFallback := func() {
		if !shouldFlushReasoningFallback(gotTextDelta, sawToolDispatch) {
			return
		}
		body, ok := reasoningFallbackBody(reasoningBuf.String())
		if !ok {
			return
		}
		gotTextDelta = true
		log.Printf("chat=%d: reasoning-only fallback len=%d runes=%d", chatID, len(body), utf8RuneCount(body))
		onChunk(body)
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		atomic.StoreInt64(&lastActivityUnix, time.Now().Unix())
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev wireEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		switch ev.Kind {
		case "text":
			// Streaming token deltas — append in place (never suffix "\n" per chunk).
			if ev.Text != "" && !isReasonixNoise(ev.Text) {
				gotTextDelta = true
				if bufferingAsk {
					askTextBuffer.WriteString(ev.Text)
				} else {
					onChunk(ev.Text)
				}
			}
		case "message":
			// Full answer at end of an agent sub-step; normally duplicates "text" deltas.
			// Never signal onComplete here — only turn_done ends the Telegram stream.
			// Mid-turn message events (tool rounds, prefill) used to finalize early and
			// drop the real answer after web_search / multi-step tools.
			if ev.Reasoning != "" {
				reasoningBuf.WriteString(ev.Reasoning)
				// DeepSeek V4 reasoning bug: model places reply in reasoning_content
				// instead of content. Try early fallback when text is missing.
				if ev.Text == "" && !gotTextDelta && !sawToolDispatch {
					if body, ok := reasoningFallbackBody(ev.Reasoning); ok {
						gotTextDelta = true
						onChunk(body)
					}
				}
			}
			if ev.Text != "" && !gotTextDelta && !isReasonixNoise(ev.Text) {
				gotTextDelta = true
				if !bufferingAsk {
					onChunk(ev.Text)
				}
			}
		case "reasoning":
			// Accumulate for end-of-turn fallback; do not stream live (too verbose).
			if ev.Text != "" {
				reasoningBuf.WriteString(ev.Text)
			} else if ev.Reasoning != "" {
				reasoningBuf.WriteString(ev.Reasoning)
			}
			continue
		case "tool_dispatch":
			if a.getMode() == ModeChat {
				if ev.Tool != nil {
					if ev.Tool.Partial {
						continue
					}
					cancelOnce.Do(func() {
						log.Printf("chat-only: blocked tool %s, cancelling turn", ev.Tool.Name)
						_ = a.postJSON(port, "/cancel", map[string]any{})
					})
				}
			} else {
				// tool mode: signal tool boundary, then send commentary
				if ev.Tool != nil && !ev.Tool.Partial && ev.Tool.Name != "" {
					sawToolDispatch = true
					// ask tool: buffer text as question, handled by ask_request event
					if ev.Tool.Name == "ask" {
						bufferingAsk = true
						askTextBuffer.Reset()
						continue
					}
					if onToolDispatch != nil {
						onToolDispatch()
					}
					emoji := toolEmoji(ev.Tool.Name)
					newLine := fmt.Sprintf("%s %s", emoji, ev.Tool.Name)
					if ev.Tool.Args != "" {
						summary := formatToolArgs(ev.Tool.Name, ev.Tool.Args)
						if summary != "" {
							newLine = summary
						}
					}

					// Consolidate consecutive same-tool calls into one line with count.
					if ev.Tool.Name == lastToolName && lastToolMsgID != 0 && toolCount > 0 {
						toolCount++
						var updated string
						if toolCount == 2 {
							// First consolidation: append (x2)
							updated = lastToolText + fmt.Sprintf(" (x%d)", toolCount)
						} else {
							// Subsequent: replace (xN) with (xN+1)
							oldSuffix := fmt.Sprintf(" (x%d)", toolCount-1)
							newSuffix := fmt.Sprintf(" (x%d)", toolCount)
							updated = strings.Replace(lastToolText, oldSuffix, newSuffix, 1)
						}
						_ = a.editCommentary(chatID, lastToolMsgID, updated)
						lastToolText = updated
						continue
					}
					// Different tool (or first tool): start a new line.
					toolCount = 1
					lastToolName = ev.Tool.Name
					if lastToolMsgID != 0 {
						// Append to existing progress message.
						fullText := lastToolText + "\n" + newLine
						_ = a.editCommentary(chatID, lastToolMsgID, fullText)
						lastToolText = fullText
					} else if onCommentary != nil {
						lastToolMsgID = onCommentary(newLine)
						lastToolText = newLine
					}
					continue
				}
			}
		case "tool_result":
			if a.getMode() != ModeChat {
				if ev.Tool != nil {
					// Reset consolidation first so hook-only skip doesn't bypass it.
					lastToolName = ""
					toolCount = 0

					// Skip hook-only noise.
					if isHookOnlyOutput(ev.Tool.Err) || isHookOnlyOutput(ev.Tool.Output) {
						continue
					}
					if lastToolMsgID != 0 {
						if ev.Tool.Err != "" {
							errMsg := stripHookMessages(ev.Tool.Err)
							if errMsg != "" && !isReasonixNoise(errMsg) {
								newText := lastToolText + "\n" + trimUTF8Bytes(errMsg, 300)
								_ = a.editCommentary(chatID, lastToolMsgID, newText)
								lastToolText = newText
							}
						}
						// Keep lastToolMsgID alive so next tool appends to this bubble.
					}
				}
			}
		case "ask_request":
			if ev.Ask != nil {
				for _, q := range ev.Ask.Questions {
					log.Printf("port=%d: ask_request q.id=%s q.prompt=%q", port, q.ID, q.Prompt)
				}
			}
			bufferingAsk = false
			askTextBuffer.Reset()
			if ev.Ask != nil && len(ev.Ask.Questions) > 0 && onAskRequest != nil {
				questions := make([]askQuestionData, len(ev.Ask.Questions))
				for i, q := range ev.Ask.Questions {
					var labels []string
					for _, opt := range q.Options {
						if opt.Label != "" {
							labels = append(labels, opt.Label)
						}

						// Attach question text
					}
					questions[i] = askQuestionData{ID: q.ID, Options: labels, Text: q.Prompt}
				}
				onAskRequest(ev.Ask.ID, questions)
			}
		case "approval_request":
			if ev.Approval != nil && ev.Approval.ID != "" {
				if onApprovalRequest != nil {
					toolName := ev.Approval.Tool
					if toolName == "" {
						toolName = "approval"
					}
					onApprovalRequest(ev.Approval.ID, toolName)
				}
			}
		case "usage":
			if ev.Usage != nil && onUsage != nil {
				log.Printf("port=%d: usage prompt=%d completion=%d total=%d cost=$%.4f", port, ev.Usage.PromptTokens, ev.Usage.CompletionTokens, ev.Usage.TotalTokens, ev.Usage.CostUSD)
				onUsage(*ev.Usage)
			}
		case "turn_done":
			log.Printf("chat=%d: turn_done err=%q", chatID, ev.Err)
			if ev.Err != "" {
				turnErr = fmt.Errorf("%s", ev.Err)
			}
			flushReasoningFallback()
			if onComplete != nil {
				log.Printf("chat=%d: onComplete via turn_done", chatID)
				onComplete()
			}
			return turnResult{err: turnErr}
		case "notice":
			if t := strings.TrimSpace(ev.Text); t != "" && !isReasonixNoise(t) {
				onChunk("\n" + t + "\n")
			}
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("chat=%d: consumeServeEvents scanner error: %v", chatID, err)
		if ctx.Err() == nil {
			return turnResult{err: err}
		}
	}
	if ctx.Err() != nil {
		return turnResult{err: ctx.Err()}
	}
	return turnResult{}
}

// openThinkTags and closeThinkTags for stripping reasoning blocks.
var openThinkTags = []string{
	"<REASONING_SCRATCHPAD>", "<think>", "<reasoning>",
	"<THINKING>", "<thinking>", "<thought>",
}
var closeThinkTags = []string{
	"</REASONING_SCRATCHPAD>", "</think>", "</reasoning>",
	"</THINKING>", "</thinking>", "</thought>",
}

// stripThinkBlocks removes content between known think/reasoning tags.
func stripThinkBlocks(s string) string {
	for i, open := range openThinkTags {
		close := closeThinkTags[i]
		for {
			start := strings.Index(s, open)
			if start < 0 {
				break
			}
			end := strings.Index(s[start+len(open):], close)
			if end < 0 {
				// No closing tag — remove from open to end
				s = s[:start]
				break
			}
			end = start + len(open) + end + len(close)
			s = s[:start] + s[end:]
		}
	}
	return s
}

func shouldFlushReasoningFallback(gotTextDelta, sawToolDispatch bool) bool {
	return !gotTextDelta && !sawToolDispatch
}

// reasoningFallbackBody returns visible text when a turn produced only reasoning.
// hasCJK returns true when s contains any CJK Unified Ideograph (Chinese/Japanese/Korean).
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// isPureEnglish returns true when s has meaningful English content but zero CJK characters.
// Reasoning/thinking from the model is nearly always English; a Chinese-locale response
// almost always contains CJK characters (even if mixed with code/terms).
func isPureEnglish(s string) bool {
	letterCount := 0
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return false
		}
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			letterCount++
		}
	}
	return letterCount > 5
}

// thinkingOpeners are phrases (lowercased) that indicate the text is self-directed
// reasoning/thinking rather than a user-facing response. When the fallback body
// begins with any of these, it is still thinking, not an answer.
var thinkingOpeners = []string{
	// Chinese self-directed analysis
	"让我", "我先", "我需要", "我们先",
	"首先", "第一步",
	"我来看看", "让我看看",
	"这个问题", "这需要",
	// English self-directed analysis
	"let me", "let's", "i need", "i want", "i should",
	"first,", "first i", "firstly",
	"the user", "the model",
}

// isLikelyThinking returns true when the text reads like internal reasoning
// rather than a direct response. Used to reject reasoning-only fallback that
// would leak the model's chain-of-thought to the user.
func isLikelyThinking(s string) bool {
	trimmed := strings.TrimSpace(s)
	lower := strings.ToLower(trimmed)
	// Check for thinking opener patterns on the first line / beginning of text.
	for _, p := range thinkingOpeners {
		if strings.HasPrefix(lower, p) {
			return true
		}
	}
	// English-only text in a reasoning-only turn is almost certainly thinking.
	if isPureEnglish(trimmed) {
		return true
	}
	return false
}

// reasoningFallbackBody returns visible text when a turn produced only reasoning.
func reasoningFallbackBody(reasoning string) (string, bool) {
	body := strings.TrimSpace(stripThinkBlocks(stripANSI(reasoning)))
	if body == "" {
		return "", false
	}
	// Split into paragraphs by double newline; prefer keeping the last paragraph
	// as the model's actual response (earlier content is chain-of-thought).
	// When the last paragraph is less than half the total, the answer likely
	// spans multiple paragraphs — return the full body instead.
	original := body
	for _, delim := range []string{"\n\n", "\n\t"} {
		if parts := strings.Split(body, delim); len(parts) > 1 {
			last := strings.TrimSpace(parts[len(parts)-1])
			if last != "" {
				body = last
				break
			}
		}
	}
	// If stripping to one paragraph loses >50% of content, the answer has
	// multiple relevant paragraphs — keep the full body.
	if len(body) < len(original)/2 && utf8RuneCount(original) > len(body)+80 {
		body = original
	}
	if body == "" || isReasonixNoise(body) || isSilenceOnly(body) || isLikelyThinking(body) {
		return "", false
	}
	return body, true
}

func utf8RuneCount(s string) int {
	return len([]rune(s))
}

// isSilenceOnly returns true if the string is just a "silence" narration
// (model telling itself to be quiet). Used to suppress empty-looking replies.
func isSilenceOnly(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return false
	}
	cleaned := strings.Trim(s, "*_~` \t")
	cleaned = strings.Trim(cleaned, ".…·•")
	cleaned = strings.TrimSpace(cleaned)
	if cleaned == "" || cleaned == "." || cleaned == "…" {
		return true
	}
	lower := strings.ToLower(cleaned)
	for _, p := range []string{"silent", "silence", "no response", "no reply"} {
		if lower == p {
			return true
		}
	}
	return false
}

// appendChunk accumulates streamed assistant text (full buffer; finalize may split across messages).
func appendChunk(buf *strings.Builder, chunk string, maxBytes int, truncated *bool) {
	clean := stripANSI(chunk)
	clean = stripThinkBlocks(clean)
	if clean == "" || isReasonixNoise(clean) {
		return
	}
	cap := maxFinalizeBytes
	if maxBytes > 0 && maxBytes < cap {
		cap = maxBytes
	}
	if buf.Len() >= cap {
		*truncated = true
		return
	}
	if buf.Len()+len(clean) > cap {
		*truncated = true
		remain := cap - buf.Len()
		if remain > 0 {
			buf.WriteString(trimUTF8Bytes(clean, remain))
		}
		return
	}
	buf.WriteString(clean)
}

