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
	"syscall"
	"time"
)

func portForChat(chatID int64) int {
	const base = 18780
	const span = 8000
	return base + int(uint64(chatID)%span)
}

func serveAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func serveBaseURL(port int) string {
	return fmt.Sprintf("http://%s", serveAddr(port))
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
	Ask *wireAsk `json:"ask,omitempty"`
}

// wireAsk mirrors reasonix serve's ask_request event (internal/serve/wire.go).
type wireAsk struct {
	ID        string            `json:"id"`
	Questions []wireAskQuestion `json:"questions"`
}

type wireAskQuestion struct {
	ID      string          `json:"id"`
	Options []wireAskOption `json:"options"`
}

type wireAskOption struct {
	Label string `json:"label"`
}

type turnResult struct {
	err error
}

func (a *App) reasonixEnv() []string {
	env := os.Environ()
	if k := os.Getenv("DEEPSEEK_API_KEY"); k != "" {
		env = append(env, "DEEPSEEK_API_KEY="+k)
	}
	// systemd ProtectHome=read-only blocks /root/.cache; keep caches under StateDir.
	cacheBase := filepath.Join(a.cfg.StateDir, "cache")
	env = append(env,
		"REASONIX_CACHE_DIR="+cacheBase,
		"XDG_CACHE_HOME="+cacheBase,
	)
	if a.cfg.Mode == ModeChat {
		env = append(env, "NO_COLOR=1", "FORCE_COLOR=0", "CI=1", "TERM=dumb")
	}
	return env
}

func (a *App) serveRunning(s *session) bool {
	s.mu.Lock()
	cmd := s.serveCmd
	s.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return cmd.ProcessState == nil
}

func (a *App) stopServe(chatID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	cmd := s.serveCmd
	s.serveCmd = nil
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		waitDone := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(5 * time.Second):
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
	s.mu.Unlock()

	if sessionPath == "" {
		sessionPath = a.state.sessionPathForChat(chatID)
		s.mu.Lock()
		s.sessionPath = sessionPath
		s.servePort = port
		s.mu.Unlock()
	}

	if _, err := os.Stat(sessionPath); os.IsNotExist(err) {
		if err := os.WriteFile(sessionPath, nil, 0o644); err != nil {
			return err
		}
	}
	msgs, users, _ := sessionStats(sessionPath)
	resume := users > 0
	if resume {
		log.Printf("chat=%d: resume session %s (%d messages, %d user turns)", chatID, sessionPath, msgs, users)
	} else {
		log.Printf("chat=%d: new session at %s", chatID, sessionPath)
	}

	args := []string{"serve", "--addr", serveAddr(port)}
	if a.cfg.Model != "" {
		args = append(args, "--model", a.cfg.Model)
	}
	// Always --resume so auto-save stays on sessionPath (not ~/.config, which is
	// read-only under systemd ProtectHome).
	args = append(args, "--resume", sessionPath)

	cmd := exec.Command(a.cfg.ReasonixBin, args...)
	cmd.Dir = wd
	cmd.Env = a.reasonixEnv()
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

	if err := waitServeReady(port, 45*time.Second); err != nil {
		a.stopServe(chatID)
		return err
	}
	// Lock plan/bypass in chat mode (tool mode leaves them on).
	a.lockServeMode(port)
	log.Printf("chat=%d: serve cwd=%s mode=%s", chatID, wd, a.cfg.Mode)
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

func (a *App) restorePersistedSessions() {
	records, err := a.state.load()
	if err != nil {
		log.Printf("warning: load persisted state: %v", err)
		return
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

func waitServeReady(port int, timeout time.Duration) error {
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

func postJSON(port int, path string, body any) error {
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

func (a *App) lockServeMode(port int) {
	if a.cfg.Mode == ModeChat {
		_ = postJSON(port, "/plan", map[string]bool{"on": false})
		_ = postJSON(port, "/bypass", map[string]bool{"on": false})
	}
	// tool mode: leave plan/bypass as-is so the agent can use tools
}

// runServeTurn submits a prompt to the long-lived reasonix serve process and
// streams SSE events until turn_done. The conversation history stays in the
// same Reasonix session file across Telegram messages and bridge restarts.
func (a *App) runServeTurn(ctx context.Context, chatID int64, prompt string, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string), onAskRequest func(askID string, questionID string, options []string), onApprovalRequest func(approvalID string, toolName string)) error {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()

	a.lockServeMode(port)

	eventsDone := make(chan turnResult, 1)
	go func() {
		eventsDone <- a.consumeServeEvents(ctx, port, onChunk, onComplete, onToolDispatch, onCommentary, onAskRequest, onApprovalRequest)
	}()

	if err := postJSON(port, "/submit", map[string]string{"input": prompt}); err != nil {
		return fmt.Errorf("submit: %w", err)
	}

	select {
	case tr := <-eventsDone:
		return tr.err
	case <-ctx.Done():
		_ = postJSON(port, "/cancel", map[string]any{})
		select {
		case tr := <-eventsDone:
			return tr.err
		case <-time.After(8 * time.Second):
			return ctx.Err()
		}
	}
}

func (a *App) consumeServeEvents(ctx context.Context, port int, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string), onAskRequest func(askID string, questionID string, options []string), onApprovalRequest func(approvalID string, toolName string)) turnResult {
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
	var cancelOnce sync.Once
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
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
			if ev.Text != "" {
				gotTextDelta = true
				onChunk(ev.Text)
			}
		case "message":
			// Full answer at end; normally duplicates "text" deltas — use as fallback only.
			if ev.Text != "" && !gotTextDelta {
				onChunk(ev.Text)
			}
			// Finalize UI as soon as the full message is known (often before turn_done).
			if ev.Text != "" && onComplete != nil {
				onComplete()
			}
		case "reasoning":
			// Hidden in Telegram — reasoning is verbose and also arrives as deltas.
			continue
		case "tool_dispatch":
			if a.cfg.Mode == ModeChat {
				if ev.Tool != nil {
					if ev.Tool.Partial {
						continue
					}
					cancelOnce.Do(func() {
						log.Printf("chat-only: blocked tool %s, cancelling turn", ev.Tool.Name)
						_ = postJSON(port, "/cancel", map[string]any{})
					})
				}
			} else {
				// tool mode: signal tool boundary, then send commentary
				if ev.Tool != nil && !ev.Tool.Partial && ev.Tool.Name != "" {
					if onToolDispatch != nil {
						onToolDispatch()
					}
					emoji := toolEmoji(ev.Tool.Name)
					msg := fmt.Sprintf("%s %s", emoji, ev.Tool.Name)
					if ev.Tool.Args != "" {
						// Trim long args for display
						args := trimUTF8Bytes(ev.Tool.Args, 200)
						msg += "\n" + args
					}
					if onCommentary != nil {
						onCommentary(msg)
					}
				}
			}
		case "tool_result":
			if a.cfg.Mode != ModeChat {
				if ev.Tool != nil {
					emoji := toolEmoji(ev.Tool.Name)
					msg := ""
					if ev.Tool.Err != "" {
						msg = fmt.Sprintf("%s ❌: %s", emoji, trimUTF8Bytes(ev.Tool.Err, 500))
					} else if ev.Tool.Output != "" {
						// Brief result preview
						preview := trimUTF8Bytes(strings.TrimSpace(ev.Tool.Output), 300)
						msg = fmt.Sprintf("%s ✅: %s", emoji, preview)
					}
					if msg != "" && onCommentary != nil {
						onCommentary(msg)
					}
				}
			}
		case "ask_request":
			log.Printf("port=%d: ask_request event", port)
			if ev.Ask != nil && len(ev.Ask.Questions) > 0 && onAskRequest != nil {
				q := ev.Ask.Questions[0]
				var labels []string
				for _, opt := range q.Options {
					if opt.Label != "" {
						labels = append(labels, opt.Label)
					}
				}
				onAskRequest(ev.Ask.ID, q.ID, labels)
			}
		case "approval_request":
			if ev.Approval != nil && ev.Approval.ID != "" {
				if onApprovalRequest != nil {
					toolName := ev.Approval.Tool
					if toolName == "" {
						toolName = "plan"
					}
					onApprovalRequest(ev.Approval.ID, toolName)
				}
			}
		case "turn_done":
			if ev.Err != "" {
				turnErr = fmt.Errorf("%s", ev.Err)
			}
			if onComplete != nil {
				onComplete()
			}
			return turnResult{err: turnErr}
		case "notice":
			if t := strings.TrimSpace(ev.Text); t != "" && !isReasonixNoise(t) {
				onChunk("\n" + t + "\n")
			}
		}
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		return turnResult{err: err}
	}
	return turnResult{err: ctx.Err()}
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

