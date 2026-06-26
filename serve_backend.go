package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var portSeq atomic.Int64 // next port offset

func portForChat(chatID int64) int {
	const base = 18780
	const span = 8000
	p := base + (int(portSeq.Add(1)) % span)
	return p
}

func serveAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func serveBaseURL(port int) string {
	return fmt.Sprintf("http://%s", serveAddr(port))
}

// isPortInUse checks whether a TCP port is already in use by trying to listen on it.
func isPortInUse(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

// readProcCWD reads a process's current working directory from /proc/PID/cwd.
func readProcCWD(pid int) string {
	link, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil {
		return ""
	}
	return link
}

// isServeStale checks whether the reasonix serve process listening on port
// was started with different arguments or CWD than what we'd launch now.
// If no reasonix serve is found on the port, returns false (not stale — caller
// should proceed to start a fresh one).
func (a *App) isServeStale(port int, expectedArgs []string, expectedCWD string) bool {
	pids := pidsListeningOnTCPPort(port)
	for _, pid := range pids {
		if pid == os.Getpid() {
			continue
		}
		cmdline := readProcCmdline(pid)
		if !isReasonixServeCmd(cmdline, a.cfg.ReasonixBin) {
			continue
		}
		// Check whether the binary at /proc/PID/exe has been replaced
		// (e.g. after a bridge upgrade). If the old binary was deleted,
		// the symlink target will contain "(deleted)".
		exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		if err != nil || strings.Contains(exe, "(deleted)") {
			return true
		}
		// Compare CWD.
		if readProcCWD(pid) != expectedCWD {
			return true
		}
		// Compare args token-by-token to avoid false matches from partial
		// substrings (e.g. --model llama3 matching --model llama3-70b).
		// Skip the binary path (cmdParts[0]) and compare against expectedArgs.
		cmdParts := strings.Fields(cmdline)
		if len(cmdParts) < 1+len(expectedArgs) {
			return true
		}
		for i, expected := range expectedArgs {
			if cmdParts[1+i] != expected {
				return true
			}
		}
		return false // all checks passed, process matches
	}
	return false // no reasonix serve found on this port
}

// localHTTPClient is a shared HTTP client for local reasonix serve communication.
// It has a 10s timeout and no Transport-level TLS since the server binds to localhost.
var localHTTPClient = &http.Client{Timeout: 10 * time.Second}

// sseClient is a separate HTTP client for long-lived SSE streams.
// It must NOT have an overall timeout — the application-level idle watchdog handles that.
var sseClient = &http.Client{}

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
	SessionCacheHitTokens  int     `json:"sessionCacheHitTokens,omitempty"`
	SessionCacheMissTokens int     `json:"sessionCacheMissTokens,omitempty"`
	SessionCost            float64 `json:"sessionCost,omitempty"`
	SessionCurrency        string  `json:"sessionCurrency,omitempty"`
	SessionPromptTokens    int     `json:"sessionPromptTokens,omitempty"`
	SessionTotalTokens     int     `json:"sessionTotalTokens,omitempty"`
}

// wireEvent mirrors reasonix serve SSE JSON (internal/serve/wire.go).
type wireEvent struct {
	Kind      string `json:"kind"`
	Seq       int64  `json:"seq,omitempty"`
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

func (a *App) reasonixEnv(chatID int64) []string {
	// Build a minimal environment for child processes — never inherit all
	// parent env vars (which may contain API keys, tokens, secrets).
	var env []string

	// Pass through safe variables that reasonix may need.
	safePrefixes := []string{
		"HOME=", "USER=", "LOGNAME=", "SHELL=",
		"PATH=", "LANG=", "LC_", "LANGUAGE=", "TZ=",
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

	// DEEPSEEK_API_KEY is forwarded via DEEPSEEK_API_KEY_FILE (see startServe)
	// to avoid leaking the secret in /proc/PID/environ.
	// systemd ProtectHome=read-only blocks /root/.cache; keep caches under StateDir.
	cacheBase := filepath.Join(a.cfg.StateDir, "cache")
	env = append(env,
		"REASONIX_CACHE_DIR="+cacheBase,
		"XDG_CACHE_HOME="+cacheBase,
	)

	if a.getMode() == ModeChat {
		env = append(env, "NO_COLOR=1", "FORCE_COLOR=0", "CI=1", "TERM=dumb")
	}

	env = append(env,
		"REASONIX_CHAT_ID="+strconv.FormatInt(chatID, 10),
		"REASONIX_CRON_TASKS_PATH="+filepath.Join(a.state.dir, "cron_tasks.json"),
	)
	return env
}

func (a *App) serveRunning(s *session) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	cmd := s.serveCmd
	_ = s.servePort
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil && cmd.ProcessState == nil {
		return true
	}
	return false
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
	port := s.servePort
	s.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		// cancelAllTasks already told serve to stop; SIGTERM immediately
		// so the serve process can flush its session JSONL and exit cleanly.
		log.Printf("chat=%d: stopping serve (pid %d)", chatID, cmd.Process.Pid)
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)

		waitDone := make(chan struct{})
		go func() {
			_, _ = cmd.Process.Wait()
			close(waitDone)
		}()
		select {
		case <-waitDone:
			log.Printf("chat=%d: serve exited cleanly", chatID)
		case <-time.After(5 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			select {
			case <-waitDone:
			case <-time.After(2 * time.Second):
				log.Printf("port=%d: timeout waiting for process to exit after kill", port)
			}
		}

		// Wait for port to be released after the process has exited.
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			if !isPortInUse(port) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
	} else if port > 0 {
		// Untracked serve (adopted from a previous bridge instance).
		// Find it by port and kill.
		log.Printf("chat=%d: stopping untracked serve on port %d", chatID, port)
		pids := pidsListeningOnTCPPort(port)
		for _, pid := range pids {
			if pid == os.Getpid() {
				continue
			}
			cmdline := readProcCmdline(pid)
			if isReasonixServeCmd(cmdline, a.cfg.ReasonixBin) {
				terminateProcessGroup(pid, 8*time.Second)
				// Wait for port to be released before returning.
				deadline := time.Now().Add(10 * time.Second)
				for time.Now().Before(deadline) {
					if !isPortInUse(port) {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
				break
			}
		}
	}
}

func (a *App) startServe(chatID int64, skipPortCheck bool) error {
	if err := a.ensureUserRulesLinked(); err != nil {
		return err
	}
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.serveCmd != nil && s.serveCmd.Process != nil && s.serveCmd.ProcessState == nil {
		s.mu.Unlock()
		return nil
	}
	workDir := a.workDir()           // tool workspace; default /root
	s.workdir = workDir
	sessionPath := s.sessionPath
	port := s.servePort
	sessionModel := s.model // per-session model override, persisted separately
	model := sessionModel
	cumPrompt := s.cumPrompt
	cumCompletion := s.cumCompletion
	cumTotal := s.cumTotal
	cumCost := s.cumCost
	cumCurrency := s.cumCurrency
	s.mu.Unlock()

	// Always use the canonical encrypted path (.jsonl.enc) so state.json never
	// stores the plain-text path (.jsonl) — the latter would cause encryptFromPlain
	// to read and overwrite the same file (self-destruction).
	sessionPath = a.state.sessionPathForChat(chatID)
	s.mu.Lock()
	s.sessionPath = sessionPath
	s.servePort = port
	s.mu.Unlock()

	// Determine the plaintext temp path for reasonix serve (it reads/writes
	// plain JSONL).  We decrypt .jsonl.enc -> .jsonl before starting serve
	// and re-encrypt after it exits.
	plainPath := a.state.sessionPathForChatPlain(chatID)

	msgs, users, err := sessionStats(sessionPath)
	if err != nil {
		// sessionStats failed — the file may be encrypted, damaged, or the key
		// has changed.  Try recovering by decrypting to the plain path first,
		// then re-stating on the plaintext copy.
		log.Printf("chat=%d: sessionStats(%s) error: %v — attempting recovery", chatID, sessionPath, err)
		if decErr := decryptToPlain(sessionPath, plainPath); decErr == nil {
			var msgs2, users2 int
			msgs2, users2, err = sessionStats(plainPath)
			if err == nil {
				msgs, users = msgs2, users2
				log.Printf("chat=%d: recovery via plaintext succeeded (%d messages, %d user turns)", chatID, msgs, users)
			}
		}
		if err != nil {
			// Recovery via decryption also failed.  Check for checkpoint data.
			ckptDir := filepath.Join(a.state.sessionsDir(), fmt.Sprintf("%d.ckpt", chatID))
			if fi, statErr := os.Stat(ckptDir); statErr == nil && fi.IsDir() {
				log.Printf("chat=%d: checkpoint %s exists — will attempt recovery from checkpoint", chatID, ckptDir)
			} else {
				log.Printf("chat=%d: no checkpoint, session data may be lost", chatID)
			}
		}
	}
	if users > 0 {
		log.Printf("chat=%d: resume session %s (%d messages, %d user turns)", chatID, sessionPath, msgs, users)
		// Decrypt the encrypted session file for the serve process.
		if err := decryptToPlain(sessionPath, plainPath); err != nil {
			log.Printf("chat=%d: decrypt session for serve: %v", chatID, err)
		}
	} else {
		// Preserve existing files by renaming to .bak.timestamp instead of deleting,
		// so session data is not irretrievably lost on a failed stat or an empty-file edge case.
		bakSuffix := ".bak." + strconv.FormatInt(time.Now().UnixMilli(), 36)
		if err := os.Rename(sessionPath, sessionPath+bakSuffix); err != nil && !os.IsNotExist(err) {
			log.Printf("rename %s -> %s: %v", sessionPath, sessionPath+bakSuffix, err)
		}
		if err := os.Rename(plainPath, plainPath+bakSuffix); err != nil && !os.IsNotExist(err) {
			log.Printf("rename %s -> %s: %v", plainPath, plainPath+bakSuffix, err)
		}
		log.Printf("chat=%d: new session at %s (preserved previous files as .bak.*)", chatID, plainPath)
	}

	args := []string{"serve", "--addr", serveAddr(port)}
	// Use per-session model if set, otherwise fall back to global config.
	if model == "" {
		model = reasonixDefaultModel
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	// Always --resume so auto-save stays on plainPath (not ~/.config/reasonix/sessions).
	// Reasonix Resume now re-applies the boot system prompt even when the file is empty.
	args = append(args, "--resume", plainPath)

	if !skipPortCheck {
		if err := a.waitServeReady(port, 2*time.Second); err == nil {
			// A serve process is already listening. Check whether it was started
			// with the same config we'd use now — if not, stop it and start fresh.
			if a.isServeStale(port, args, workDir) {
				log.Printf("chat=%d: existing serve stale (config mismatch), restarting", chatID)
				a.stopServe(chatID)
				// Wait for the old process to release the port.
				for i := 0; i < 30; i++ {
					if err := a.waitServeReady(port, 100*time.Millisecond); err != nil {
						break
					}
					time.Sleep(100 * time.Millisecond)
				}
			} else {
				log.Printf("chat=%d: adopting existing reasonix serve on %s", chatID, serveAddr(port))
				return nil
			}
		}
	}

	// Port availability check: if the assigned port is already in use,
	// try subsequent ports before giving up.
	portOK := false
	for i := 0; i < 10; i++ {
		if !isPortInUse(port) {
			portOK = true
			break
		}
		if i < 9 {
			log.Printf("chat=%d: port %d in use, trying %d", chatID, port, port+1)
			port++
		}
	}
	if !portOK {
		return fmt.Errorf("chat=%d: no available TCP port after 10 attempts", chatID)
	}
	// Persist the chosen port so future restarts use the same offset.
	s.mu.Lock()
	s.servePort = port
	s.mu.Unlock()

	cmd := exec.Command(a.cfg.ReasonixBin, args...)
	cmd.Dir = workDir
	env := a.reasonixEnv(chatID)
	// Create temp file for DEEPSEEK_API_KEY to avoid exposure in /proc/PID/environ.
	var keyFile string
	if k := a.cfg.DeepSeekKey; k != "" {
		tmpf, err := os.CreateTemp("", "deepseek-key-*")
		if err == nil {
			if _, err := tmpf.WriteString(k); err == nil {
				tmpf.Close()
				if err := os.Chmod(tmpf.Name(), 0600); err == nil {
					keyFile = tmpf.Name()
					env = append(env, "DEEPSEEK_API_KEY_FILE="+keyFile)
				}
			} else {
				tmpf.Close()
			}
		}
		// Reasonix reads the key from DEEPSEEK_API_KEY_FILE (set above).
	}
	cmd.Env = env
	// Pipe stderr through log.Printf so diagnostics go through the sanitized log pipeline.
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		for scanner.Scan() {
			log.Printf("[serve stderr] %s", scanner.Text())
		}
		if err := scanner.Err(); err != nil {
			log.Printf("[serve stderr] pipe read error: %v", err)
		}
	}()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: 0}

	if err := cmd.Start(); err != nil {
		if keyFile != "" {
			if err := os.Remove(keyFile); err != nil {
			log.Printf("remove %s: %v", keyFile, err)
		}
		}
		return fmt.Errorf("start reasonix serve: %w", err)
	}
	// Clean up temp key file after the child has started (it reads it at startup).
	if keyFile != "" {
		defer func() {
			if err := os.Remove(keyFile); err != nil {
				log.Printf("remove %s: %v", keyFile, err)
			}
		}()
	}

	s.mu.Lock()
	s.serveCmd = cmd
	s.sessionPath = sessionPath
	s.servePort = port
	s.encryptDone = make(chan struct{})
	s.mu.Unlock()

	go func() {
		defer close(s.encryptDone)
		err := cmd.Wait()
		s.mu.Lock()
		if s.serveCmd == cmd {
			s.serveCmd = nil
		}
		s.mu.Unlock()
		if err != nil {
			log.Printf("chat=%d: reasonix serve exited: %v", chatID, err)
		}
		// Re-encrypt the plaintext session file back to encrypted storage.
		if err := encryptFromPlain(plainPath, sessionPath); err != nil {
			log.Printf("chat=%d: re-encrypt session after serve exit: %v", chatID, err)
		}
	}()

	if err := a.waitServeReady(port, 45*time.Second); err != nil {
		a.stopServe(chatID)
		return err
	}
	log.Printf("chat=%d: serve cwd=%s mode=%s", chatID, workDir, a.getMode())
	if err := a.state.upsert(chatRecord{
		ChatID:      chatID,
		Workdir:     workDir,
		SessionPath: sessionPath,
		Port:        port,
		Model:       sessionModel,
		CumPrompt:   cumPrompt,
		CumComplete: cumCompletion,
		CumTotal:    cumTotal,
		CumCost:     cumCost,
		CumCurrency: cumCurrency,
		HMACKey:     base64.StdEncoding.EncodeToString(s.hmacKey),
	}); err != nil {
		log.Printf("chat=%d: persist state failed: %v", chatID, err)
	}
	log.Printf("chat=%d: reasonix serve ready on %s session=%s", chatID, serveAddr(port), sessionPath)
	return nil
}

func (a *App) ensureServe(chatID int64) error {
	s := a.getOrCreateSession(chatID)
	if a.serveRunning(s) {
		// Process is alive but may still be binding the port (startup race
		// between restore goroutine's cmd.Start and waitServeReady).
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port > 0 {
			return a.waitServeReady(port, 10*time.Second)
		}
		return nil
	}
	return a.startServe(chatID, true)
}

// startServeHealthCheck periodically checks all serve processes are alive.
// Runs every 60s; restarts any that have died, with exponential backoff.
func (a *App) startServeHealthCheck() {
	go func() {
		ticker := time.NewTicker(60 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
			case <-a.healthCheckStop:
				log.Printf("health: stop signal received, exiting health check")
				return
			}
			a.sessMu.Lock()
			type deadChat struct {
				id  int64
				s   *session
			}
			var dead []deadChat
			for chatID, s := range a.sess {
				s.mu.Lock()
				cmd := s.serveCmd
				port := s.servePort
				s.mu.Unlock()
				// Check if process is dead but port not listening
				alive := cmd != nil && cmd.Process != nil && cmd.ProcessState == nil
				listening := port > 0 && a.waitServeReady(port, 2*time.Second) == nil
				if !alive && !listening {
					dead = append(dead, deadChat{chatID, s})
				}
			}
			a.sessMu.Unlock()
			for _, dc := range dead {
				chatID := dc.id
				s := dc.s
				s.mu.Lock()
				count := s.serveRestartCount
				s.mu.Unlock()
				if count >= 10 {
					log.Printf("health: chat=%d serve process dead, %d consecutive failures, giving up", chatID, count)
					continue
				}
				backoff := time.Duration(1 << uint(count)) * time.Second // 1s, 2s, 4s, 8s, ...
				if backoff > 300*time.Second {
					backoff = 300 * time.Second
				}
				// Ensure minimum gap between restarts.
				s.mu.Lock()
				elapsed := time.Since(s.serveLastRestart)
				s.mu.Unlock()
				if elapsed < backoff {
					log.Printf("health: chat=%d backoff %v remaining (%d consecutive failures)", chatID, backoff-elapsed, count)
					continue
				}
				log.Printf("health: chat=%d serve process dead, restarting (attempt %d)", chatID, count+1)
				s.mu.Lock()
				s.serveLastRestart = time.Now()
				s.mu.Unlock()
				if err := a.startServe(chatID, false); err != nil {
					s.mu.Lock()
					s.serveRestartCount++
					log.Printf("health: chat=%d restart failed: %v (consecutive failures: %d)", chatID, err, s.serveRestartCount)
					s.mu.Unlock()
				} else {
					s.mu.Lock()
					s.serveRestartCount = 0
					s.mu.Unlock()
				}
			}
		}
	}()
}

func (a *App) restorePersistedSessions() {
	a.state.migrateOldSessions()
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
	const maxConcurrentRestores = 5
	sem := make(chan struct{}, maxConcurrentRestores)
	for _, rec := range records {
		s := a.getOrCreateSession(rec.ChatID)
		s.mu.Lock()
		s.workdir = a.workDir()
		s.sessionPath = rec.SessionPath
		if s.sessionPath == "" || !strings.HasSuffix(s.sessionPath, ".jsonl.enc") {
			s.sessionPath = a.state.sessionPathForChat(rec.ChatID)
		}
		s.servePort = rec.Port
		if s.servePort == 0 {
			s.servePort = portForChat(rec.ChatID)
		}
		s.model = rec.Model
		s.cumPrompt = rec.CumPrompt
		s.cumCompletion = rec.CumComplete
		s.cumTotal = rec.CumTotal
		s.cumCost = rec.CumCost
		s.cumCurrency = rec.CumCurrency
		// Restore HMAC key from persisted state.
		if rec.HMACKey != "" {
			if decoded, err := base64.StdEncoding.DecodeString(rec.HMACKey); err == nil && len(decoded) == 32 {
				s.hmacKey = decoded
			}
		}
		s.mu.Unlock()
		sem <- struct{}{}
		go func(chatID int64) {
			defer func() { <-sem }()
			if err := a.startServe(chatID, false); err != nil {
				log.Printf("chat=%d: restore serve failed (will retry on next message): %v", chatID, err)
			}
		}(rec.ChatID)
	}
	if len(records) > 0 {
		log.Printf("restoring %d persisted reasonix session(s)", len(records))
	}
}

// fetchServeModelLabel queries the Reasonix serve /status endpoint and returns
// the model label (e.g. "mimo-v2.5"). Returns "" on failure.
func (a *App) fetchServeModelLabel(port int) string {
	if port == 0 {
		return ""
	}
	url := serveBaseURL(port) + "/status"
	resp, err := localHTTPClient.Get(url)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var status struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return ""
	}
	return status.Label
}

// fetchServeCache queries the Reasonix serve /status endpoint and returns
// session-cumulative cache hit/miss token counts. Unlike s.lastUsage (which
// reflects only the current agent's state and resets on serve restart), these
// come from Controller.SessionCache() — the true session-wide aggregates.
// Returns (0, 0) on failure.
func (a *App) fetchServeCache(port int) (hit, miss int) {
	if port == 0 {
		return 0, 0
	}
	url := serveBaseURL(port) + "/status"
	resp, err := localHTTPClient.Get(url)
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	var status struct {
		Hit  int `json:"cacheHit"`
		Miss int `json:"cacheMiss"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return 0, 0
	}
	return status.Hit, status.Miss
}

func (a *App) waitServeReady(port int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	url := serveBaseURL(port) + "/status"
	client := localHTTPClient
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			if resp.StatusCode == http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					log.Printf("waitServeReady: read body: %v", err)
				}
				resp.Body.Close()
				if strings.Contains(string(body), "label") {
					return nil
				}
			} else {
				resp.Body.Close()
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("reasonix serve not ready at %s", url)
}

func (a *App) postJSON(ctx context.Context, port int, path string, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serveBaseURL(port)+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := localHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("postJSON: read error response body: %v", err)
		}
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

	if err := a.postJSON(ctx, port, "/submit", map[string]string{"input": prompt}); err != nil {
		cancel()
		return fmt.Errorf("submit: %w", err)
	}

	select {
	case tr := <-eventsDone:
		return tr.err
	case <-ctx.Done():
		cancelCtx, cancelCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancelCancel()
		_ = a.postJSON(cancelCtx, port, "/cancel", map[string]any{})
		select {
		case tr := <-eventsDone:
			return tr.err
		case <-time.After(8 * time.Second):
			return ctx.Err()
		}
	}
}

// errNonSSEResponse is returned when the serve endpoint returns a response with
// an unexpected Content-Type (not text/event-stream). This is a fatal error that
// should not be retried, since retrying would produce the same non-SSE response.
var errNonSSEResponse = errors.New("serve returned non-SSE response")

func (a *App) connectAndConsumeSSE(ctx context.Context, chatID int64, port int, lastSeq int64, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string) int, onAskRequest func(askID string, questions []askQuestionData), onApprovalRequest func(approvalID string, toolName string), onUsage func(wireUsage)) (int64, error) {
	eventsURL := serveBaseURL(port) + "/events"
	if lastSeq > 0 {
		eventsURL = fmt.Sprintf("%s/events?offset=%d", serveBaseURL(port), lastSeq)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := sseClient.Do(req)
	if err != nil {
		return 0, fmt.Errorf("reasonix events: %w", err)
	}
	var closeBodyOnce sync.Once
	closeBody := func() {
		closeBodyOnce.Do(func() {
			resp.Body.Close()
		})
	}
	defer closeBody()

	// Verify this is an SSE stream; non-SSE responses (e.g. HTML error pages
	// from a misconfigured serve) are fatal and should not be retried.
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		closeBody()
		return 0, fmt.Errorf("%w: %q (URL: %s)", errNonSSEResponse, ct, eventsURL)
	}

	ctxDoneChan := make(chan struct{})
	defer close(ctxDoneChan)
	go func() {
		select {
		case <-ctx.Done():
			closeBody()
		case <-ctxDoneChan:
		}
	}()

	var turnErr error
	var gotTextDelta bool
	var cancelOnce sync.Once
	var lastToolMsgID int
	var lastToolText string // raw text of last tool dispatch (for appending result)
	var lastToolName string // last dispatched tool name (for consolidation)
	var lastToolBase string // base display text of last tool (without (xN) suffix)
	var toolCount int      // consecutive same-tool calls
	var bufferingAsk bool // true while accumulating question text for ask tool
	var askTextBuffer strings.Builder

	// SSE idle watchdog: close body if no data for 2 min (Hermes-inspired).
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
				if elapsed > 120 {
					log.Printf("port=%d: SSE idle for %ds, closing stream", port, elapsed)
					closeBody()
					return
				}
			case <-watchdogCtx.Done():
				return
			}
		}
	}()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	consecutiveErrors := 0
	for sc.Scan() {
		// If context was cancelled (pre-empted by a newer turn), stop processing events
		// immediately to avoid duplicate messages from concurrent goroutines.
		select {
		case <-ctx.Done():
			log.Printf("port=%d: SSE context cancelled, stopping event processing", port)
			return lastSeq, ctx.Err()
		default:
		}
		atomic.StoreInt64(&lastActivityUnix, time.Now().Unix())
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var ev wireEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			consecutiveErrors++
			log.Printf("SSE parse error for chat=%d: %v, payload=%.200s", chatID, err, payload)
			if consecutiveErrors >= 5 {
				log.Printf("SSE: %d consecutive parse errors for chat=%d, breaking connection", consecutiveErrors, chatID)
				break
			}
			continue
		}
		consecutiveErrors = 0

		// Track highest seq for offset-based reconnection.
		if ev.Seq > lastSeq {
			lastSeq = ev.Seq
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
			// drop the real answer after web_search / multi-step tools. The agent's own
			// empty-text recovery handles reasoning-only turns — no bridge-side fallback.
			if ev.Text != "" && !gotTextDelta && !isReasonixNoise(ev.Text) {
				gotTextDelta = true
				if !bufferingAsk {
					onChunk(ev.Text)
				}
			}
		case "reasoning":
			// The agent's own empty-text recovery handles reasoning-only turns.
			// Do not buffer or forward reasoning content — it is internal chain-of-thought.
			continue
		case "tool_dispatch":
			if a.getMode() == ModeChat {
				if ev.Tool != nil {
					if ev.Tool.Partial {
						continue
					}
					cancelOnce.Do(func() {
						log.Printf("chat-only: blocked tool %s, cancelling turn", ev.Tool.Name)
						_ = a.postJSON(ctx, port, "/cancel", map[string]any{})
					})
				}
			} else {
				// tool mode: signal tool boundary, then send commentary
				if ev.Tool != nil && !ev.Tool.Partial && ev.Tool.Name != "" {
					// ask tool: buffer text as question, handled by ask_request event
					if ev.Tool.Name == "ask" {
						bufferingAsk = true
						askTextBuffer.Reset()
						continue
					}
					if onToolDispatch != nil {
						onToolDispatch()
					}
					newLine := toolDisplayLine(ev.Tool.Name, ev.Tool.Args)

					// Consolidate consecutive same-tool calls (same base display text) into one line with count.
					if ev.Tool.Name == lastToolName && newLine == lastToolBase && lastToolMsgID != 0 && toolCount > 0 {
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
						if newID, err := a.editCommentary(chatID, lastToolMsgID, updated); newID != 0 {
							lastToolMsgID = newID
						} else if err != nil {
							log.Printf("chat=%d: commentary consolidation failed: %v", chatID, err)
						}
						lastToolText = updated
						continue
					}
					// Different tool (or first tool, or same name but different display text): always send a new bubble.
					toolCount = 1
					lastToolName = ev.Tool.Name
					lastToolBase = newLine
					if onCommentary != nil {
						lastToolMsgID = onCommentary(newLine)
						lastToolText = newLine
					}
					continue
				}
			}
		case "tool_result":
			if a.getMode() != ModeChat {
				if ev.Tool != nil {
					// Skip hook-only noise.
					if isHookOnlyOutput(ev.Tool.Err) || isHookOnlyOutput(ev.Tool.Output) {
						continue
					}
					if lastToolMsgID != 0 {
						if ev.Tool.Err != "" {
							errMsg := stripHookMessages(ev.Tool.Err)
							if errMsg != "" && !isReasonixNoise(errMsg) {
								newText := lastToolText + "\n" + trimUTF8Bytes(errMsg, 300)
								if newID, e := a.editCommentary(chatID, lastToolMsgID, newText); newID != 0 {
									lastToolMsgID = newID
								} else if e != nil {
									log.Printf("chat=%d: commentary tool_result failed: %v", chatID, e)
								}
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
					log.Printf("port=%d: ask_request q.id=%s q.prompt=%s", port, q.ID, logPreview(q.Prompt, 100))
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
			if onComplete != nil {
				log.Printf("chat=%d: onComplete via turn_done", chatID)
				onComplete()
			}
			return lastSeq, turnErr
		case "notice":
			if t := strings.TrimSpace(ev.Text); t != "" && !isReasonixNoise(t) {
				key := fmt.Sprintf("%d|%s", chatID, t)
				a.noticeMu.Lock()
				if a.lastNotice == nil {
					a.lastNotice = make(map[string]time.Time)
				}
				last, ok := a.lastNotice[key]
				skip := ok && time.Since(last) < 5*time.Second
				if !skip {
					a.lastNotice[key] = time.Now()
				}
				a.noticeMu.Unlock()
				if skip {
					continue
				}
				onChunk("\n" + t + "\n")
			}
		}
	}
	if err := sc.Err(); err != nil {
		log.Printf("chat=%d: consumeServeEvents scanner error: %v", chatID, err)
		if ctx.Err() == nil {
			return lastSeq, err
		}
	}
	if ctx.Err() != nil {
		return lastSeq, ctx.Err()
	}
	return lastSeq, nil
}

// consumeServeEvents connects to the serve SSE stream and processes events.
// It retries transient connection failures up to 3 times with exponential
// backoff (500ms, 1s, 2s). Fatal errors (non-SSE response, context
// cancellation, dead serve process) are returned immediately without retry.
func (a *App) consumeServeEvents(ctx context.Context, chatID int64, port int, onChunk func(string), onComplete func(), onToolDispatch func(), onCommentary func(string) int, onAskRequest func(askID string, questions []askQuestionData), onApprovalRequest func(approvalID string, toolName string), onUsage func(wireUsage)) turnResult {
	const maxRetries = 3
	backoffs := []time.Duration{500 * time.Millisecond, 5 * time.Second, 30 * time.Second}
	var offset int64

	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Check if the turn was canceled (e.g. pre-empted by a newer message).
			if ctx.Err() != nil {
				return turnResult{err: fmt.Errorf("serve turn canceled during retry: %w", ctx.Err())}
			}
			// Check that the serve process is still alive before reconnecting.
			if err := a.waitServeReady(port, 2*time.Second); err != nil {
				return turnResult{err: fmt.Errorf("serve process dead, cannot retry: %w", err)}
			}
			log.Printf("chat=%d: SSE reconnect attempt %d/%d after %v (offset=%d)", chatID, attempt, maxRetries, backoffs[attempt-1], offset)
			time.Sleep(backoffs[attempt-1])
		}

		lastSeq, err := a.connectAndConsumeSSE(ctx, chatID, port, offset, onChunk, onComplete, onToolDispatch, onCommentary, onAskRequest, onApprovalRequest, onUsage)
		if lastSeq > 0 {
			offset = lastSeq
		}
		if err == nil {
			return turnResult{}
		}

		// Fatal errors: non-SSE response or context cancellation — do not retry.
		if errors.Is(err, errNonSSEResponse) || errors.Is(err, context.Canceled) {
			return turnResult{err: err}
		}

		log.Printf("chat=%d: SSE connection lost (attempt %d/%d, offset=%d): %v", chatID, attempt+1, maxRetries+1, offset, err)
	}
	log.Printf("chat=%d: SSE connection failed after %d retries, restarting serve process", chatID, maxRetries+1)
	a.stopServe(chatID)
	if startErr := a.startServe(chatID, true); startErr != nil {
		log.Printf("chat=%d: failed to restart serve after SSE failure: %v", chatID, startErr)
	}
	return turnResult{err: fmt.Errorf("SSE connection failed after %d retries", maxRetries+1)}
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
				// No closing tag — could be split across chunks.
				// Only remove the opening tag itself, keep everything after.
				s = s[:start] + s[start+len(open):]
				continue
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

// leakDecision is the outcome of probing the start of a text stream for
// thinking-content leakage (model putting chain-of-thought into the text
// channel instead of the reasoning channel).
type leakDecision int

const (
	leakUndecided leakDecision = iota // not enough text yet — keep probing
	leakKeep                          // normal content — flush probe and stop probing
	leakDrop                          // thinking leak — drop this segment
)

// thinkingLeakOpeners are English chain-of-thought sentence starters that
// indicate the model is reasoning aloud rather than answering. The bot's
// system rules require Chinese replies, so English text at the start of a
// turn is a strong leak signal. Prefix-matched, case-insensitive.
var thinkingLeakOpeners = []string{
	"let me", "let's", "i need to", "i need", "i'll", "i should",
	"i have to", "i want to", "i'm going to", "first,", "now i",
	"looking at", "checking", "let me check", "i'll check",
	"let's see", "okay, let", "ok, let", "alright, let",
	"i should check", "i should look", "i should verify",
	"let me look", "let me see", "let me think",
	"i'll look", "i'll see", "i'll think",
	"i need to check", "i need to look", "i need to verify",
	"let me verify", "let me confirm", "let me understand",
}

// hasChineseRune reports whether s contains any CJK Unified Ideograph.
func hasChineseRune(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// detectThinkingLeak examines the accumulated probe text and decides whether
// it is a thinking-leak (English chain-of-thought), normal content, or still
// undetermined. The probe is limited to ~300 bytes; once that is exceeded
// without a leak signal, the content is kept.
func detectThinkingLeak(probe string, isEOF bool) leakDecision {
	// Strip ANSI/noise before examining.
	clean := stripANSI(probe)
	clean = stripThinkBlocks(clean)
	trimmed := strings.TrimSpace(clean)
	if trimmed == "" {
		return leakUndecided
	}
	// Chinese present → normal reply (rules require Chinese). Flush and keep.
	if hasChineseRune(trimmed) {
		return leakKeep
	}
	lower := strings.ToLower(trimmed)
	for _, opener := range thinkingLeakOpeners {
		if strings.HasPrefix(lower, opener) {
			return leakDrop
		}
	}
	// Probe limit: if we have enough text and no leak signal, keep it.
	// 300 bytes of pure ASCII without a Chinese rune or leak opener is
	// likely code or a non-thinking English reply — let it through.
	if len(trimmed) > 300 {
		return leakKeep
	}
	if isEOF {
		return leakKeep
	}
	return leakUndecided
}

// appendChunk accumulates streamed assistant text (full buffer; finalize may split across messages).
func appendChunk(buf *strings.Builder, chunk string, maxBytes int, truncated *bool) {
	clean := stripANSI(chunk)
	clean = stripThinkBlocks(clean)
	if clean == "" {
		return
	}
	// Only filter complete lines (ending with \n) as reasonix noise; incomplete
	// chunks that arrive mid-stream are always kept to avoid truncation false positives.
	if strings.HasSuffix(clean, "\n") && isReasonixNoise(clean) {
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

// stripHookMessages removes RTK hook interception messages from tool output.
func stripHookMessages(output string) string {
	// Hook interceptions look like:
	//   hook <name> intercepted [args] → <reason>
	//   hook <name> — intercepted: <reason>
	// or multiline variants covering the entire tool output.
	lines := strings.Split(output, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Single-line hook interception
		if strings.HasPrefix(trimmed, "hook ") && strings.Contains(trimmed, "intercepted") {
			continue
		}
		kept = append(kept, line)
	}
	result := strings.TrimSpace(strings.Join(kept, "\n"))
	if result == "" {
		return ""
	}
	return result
}

// stripBackgroundJobs removes reasonix background-job lifecycle blocks from text.
// The actual format from reasonix serve is:
//
//	background bash started: bash-1 (sleep 10)
//	background bash finished: bash-1
//
// This strips any "background <type> <action>: <id>" line.
func stripBackgroundJobs(text string) string {
	if !strings.Contains(text, "background") {
		return text
	}
	re := regexp.MustCompile(`(?m)^\s*background\s+\S+\s+(started|finished|failed|killed):\s+\S+.*(\n|$)`)
	return strings.TrimSpace(re.ReplaceAllString(text, ""))
}

// stripErrorLines removes known error/diagnostic lines from text.
// These are reasonix internal messages that should not reach the end user.
func stripErrorLines(text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			kept = append(kept, line)
			continue
		}
		// unknown ref errors from ctx_read / ctx_search tool (store.go)
		if strings.Contains(trimmed, "unknown ref") {
			continue
		}
		// [ctx] summary lines from ctx_read output
		if strings.HasPrefix(trimmed, "[ctx]") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func toolDisplayLine(toolName, argsJSON string) string {
	// Parse args once.
	var args map[string]any
	if argsJSON != "" {
		_ = json.Unmarshal([]byte(argsJSON), &args)
	}

	// Helper to extract string arg.
	str := func(key string) string {
		if args == nil {
			return ""
		}
		if v, ok := args[key].(string); ok {
			return v
		}
		return ""
	}

	// Unified emoji + summary per tool.
	switch toolName {
	case "bash":
		cmd := strings.TrimSpace(str("command"))
		if cmd == "" {
			return "💻 bash"
		}
		const capRunes = 300
		runes := []rune(cmd)
		if len(runes) > capRunes {
			cmd = string(runes[:capRunes]) + "…"
		}
		return fmt.Sprintf("💻 bash\n\n```\n%s\n```", cmd)
	case "python", "python3", "execute_code", "code":
		return "🐍 " + toolName
	case "read_file", "cat":
		if p := str("path"); p != "" && len(p) > 3 {
			return "📖 " + trimUTF8Bytes(p, 80)
		}
		return "📖 read_file"
	case "write_file", "edit_file", "multi_edit":
		if p := str("path"); p != "" && len(p) > 3 {
			return "✍️ " + trimUTF8Bytes(p, 80)
		}
		return "✍️ write"
	case "grep", "search_files":
		if q := str("pattern"); q != "" && len(q) > 1 {
			return "🔎 " + trimUTF8Bytes(q, 80)
		}
		return "🔎 grep"
	case "glob", "ls":
		if p := str("path"); p != "" && len(p) > 2 {
			return "📁 " + trimUTF8Bytes(p, 80)
		}
		if p := str("pattern"); p != "" && len(p) > 2 {
			return "📁 " + p
		}
		return "📁 ls"
	// (deleted codegraph_search)
	// (deleted codegraph_callees/callers/impact)
	// (deleted codegraph_context)
	// (deleted codegraph_explore/trace)
	// (deleted codegraph_files)
	case "search_web", "web_search":
		if q := str("query"); q != "" {
			return "🌐 " + trimUTF8Bytes(q, 80)
		}
		return "🌐 search"
	case "web_fetch", "read_url":
		if u := str("url"); u != "" {
			return "🌐 " + trimUTF8Bytes(u, 80)
		}
		return "🌐 fetch"
	case "curl", "wget":
		if u := str("url"); u != "" {
			return "📄 " + trimUTF8Bytes(u, 80)
		}
		return "📄 curl"
	case "remember", "memory_save", "memory":
		if t := str("title"); t != "" {
			return "🧠 " + t
		}
		return "🧠 remember"
	case "forget":
		return "🗑️ forget"
	case "note":
		if p := str("path"); p != "" {
			return "📝 " + trimUTF8Bytes(p, 80)
		}
		if k := str("kind"); k != "" {
			return "📝 " + k
		}
		return "📝 note"
	case "ctx_read":
		if ref := str("ref"); ref != "" {
			return "📑 ctx_read(" + ref + ")"
		}
		return "📑 ctx_read"
	case "ctx_search":
		if p := str("pattern"); p != "" {
			return "🔍 ctx_search(" + trimUTF8Bytes(p, 60) + ")"
		}
		return "🔍 ctx_search"
	case "ctx_run":
		return "💻 ctx_run"
	case "ctx_index":
		if p := str("path"); p != "" {
			return "📑 " + trimUTF8Bytes(p, 80)
		}
		return "📑 ctx_index"
	case "audit_finish":
		return "📋 audit"
	case "delete_range":
		if p := str("path"); p != "" {
			return "🗑️ " + trimUTF8Bytes(p, 80)
		}
		return "🗑️ delete_range"
	case "delete_symbol":
		if n := str("name"); n != "" {
			return "🗑️ " + n
		}
		return "🗑️ delete_symbol"
	case "notebook_edit":
		if p := str("path"); p != "" {
			return "📓 " + trimUTF8Bytes(p, 80)
		}
		return "📓 notebook"
	case "bash_output":
		if j := str("job_id"); j != "" {
			return "⏱ bash_output(" + j + ")"
		}
		return "⏱ bash_output"
	case "wait":
		return "⏱ wait"
	case "kill_shell":
		if j := str("job_id"); j != "" {
			return "⏱ kill_shell(" + j + ")"
		}
		return "⏱ kill_shell"
	case "list_scheduled_tasks":
		return "📅 scheduled"
	case "list_sessions":
		return "💬 sessions"
	case "read_session":
		return "📜 read_session"
	case "run_skill", "install_skill", "install_source", "slash_command":
		return "📚 " + toolName
	case "read_skill":
		return "📖 read_skill"
	case "task":
		if desc := str("description"); desc != "" {
			return fmt.Sprintf("🤖 task(%s)", desc)
		}
		return "🤖 task"
	case "explore":
		if task := str("task"); task != "" {
			return "🔍 explore(" + trimUTF8Bytes(task, 60) + ")"
		}
		return "🔍 explore"
	case "research":
		if task := str("task"); task != "" {
			return "🧪 research(" + trimUTF8Bytes(task, 60) + ")"
		}
		return "🧪 research"
	case "review":
		if task := str("task"); task != "" {
			return "🧐 review(" + trimUTF8Bytes(task, 60) + ")"
		}
		return "🧐 review"
	case "security_review":
		if task := str("task"); task != "" {
			return "🛡️ security(" + trimUTF8Bytes(task, 60) + ")"
		}
		return "🛡️ security"
	case "gh", "git":
		return "🔀 " + toolName
	case "docker":
		return "🐳 docker"
	case "systemctl", "service":
		return "⚙️ " + toolName
	case "ask":
		return "❓ ask"
	case "lsp_definition":
		if sym := str("symbol"); sym != "" {
			return "🎯 definition(" + sym + ")"
		}
		return "🎯 definition"
	case "lsp_diagnostics":
		if f := str("file"); f != "" {
			return "⚠️ diagnostics(" + trimUTF8Bytes(f, 80) + ")"
		}
		return "⚠️ diagnostics"
	case "lsp_hover":
		if sym := str("symbol"); sym != "" {
			return "🖱️ hover(" + sym + ")"
		}
		return "🖱️ hover"
	case "lsp_references":
		if sym := str("symbol"); sym != "" {
			return "🔗 references(" + sym + ")"
		}
		return "🔗 references"
	default:
		// MCP tools: match by prefix.
		switch {
		case strings.HasPrefix(toolName, "mcp_cf-bindings_"):
			return "☁️ " + strings.TrimPrefix(toolName, "mcp_cf-bindings_")
		case strings.HasPrefix(toolName, "mcp_cf-observability_"):
			return "📊 " + strings.TrimPrefix(toolName, "mcp_cf-observability_")
		case strings.HasPrefix(toolName, "mcp_cf-builds_"):
			return "🔨 " + strings.TrimPrefix(toolName, "mcp_cf-builds_")
		case strings.HasPrefix(toolName, "mcp_cf-docs_"):
			return "📖 " + strings.TrimPrefix(toolName, "mcp_cf-docs_")
		case strings.HasPrefix(toolName, "mcp_jina_"):
			return "🌐 " + strings.TrimPrefix(toolName, "mcp_jina_")
		case strings.HasPrefix(toolName, "mcp_codegraph_"):
			return "🔬 " + strings.TrimPrefix(toolName, "mcp_codegraph_")
		default:
			// Fallback: try to find a string arg for display.
			if args != nil {
				for _, v := range args {
					if s, ok := v.(string); ok && len(s) > 0 && len(s) < 100 {
						return "⚡ " + trimUTF8Bytes(s, 80)
					}
				}
			}
			return "⚡ " + toolName
		}
	}
}



// formatToolResult formats tool output for compact Telegram display.
func isHookOnlyOutput(output string) bool {
	if output == "" {
		return false
	}
	return strings.TrimSpace(stripHookMessages(output)) == ""
}
