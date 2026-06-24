package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

type PlatformBridge interface {
	GetBot() *tgbotapi.BotAPI
	RegisterCommands() error
	GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel
}

type TelegramBridge struct {
	bot *tgbotapi.BotAPI
	cfg *Config
}

func NewTelegramBridge(cfg *Config) (PlatformBridge, error) {
	bot, err := newBotWithRetry(cfg)
	if err != nil {
		return nil, err
	}
	bot.Debug = false

	sanitizer := NewTokenSanitizer(cfg.BotToken)
	innerTransport := &tokenRedactingTransport{
		inner:     http.DefaultTransport,
		sanitizer: sanitizer,
	}
	bot.Client = &http.Client{
		Transport: NewTelegramFallbackTransport(innerTransport, os.Getenv("TELEGRAM_FALLBACK_IPS"), sanitizer),
		Timeout:   30 * time.Second,
	}

	log.Printf("reasonix-telegram %s — authorized as @%s (id=%d)", version, bot.Self.UserName, bot.Self.ID)

	return &TelegramBridge{
		bot: bot,
		cfg: cfg,
	}, nil
}

func (t *TelegramBridge) GetBot() *tgbotapi.BotAPI {
	return t.bot
}

func (t *TelegramBridge) RegisterCommands() error {
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
		{Command: "cron", Description: "创建定时任务：/cron [表达式] [Prompt]"},
		{Command: "cron_list", Description: "查看定时任务列表"},
		{Command: "cron_del", Description: "删除定时任务：/cron_del [ID]"},
	}
	_, err := t.bot.Request(tgbotapi.NewSetMyCommands(cmds...))
	return err
}

func (t *TelegramBridge) GetUpdatesChan(config tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	return t.bot.GetUpdatesChan(config)
}

func newBotWithRetry(cfg *Config) (*tgbotapi.BotAPI, error) {
	const (
		maxRetries = 5
		baseDelay  = 1 * time.Second
		maxDelay   = 30 * time.Second
	)

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			delay := time.Duration(1<<(attempt-1)) * baseDelay // 1s, 2s, 4s, 8s, 16s
			if delay > maxDelay {
				delay = maxDelay
			}
			log.Printf("telegram auth retry %d/%d after %v (last error: %v)",
				attempt, maxRetries, delay, redactSecrets(lastErr.Error(), cfg.secrets))

			discoverFallbackIPs()

			time.Sleep(delay)
		}

		bot, err := tgbotapi.NewBotAPI(cfg.BotToken)
		if err == nil {
			return bot, nil
		}
		lastErr = err

		if isPermanentAuthError(err) {
			return nil, fmt.Errorf("permanent auth failure: %w", err)
		}
		log.Printf("telegram auth temporary failure: %v", redactSecrets(err.Error(), cfg.secrets))
	}
	return nil, fmt.Errorf("telegram auth failed after %d attempts: %w", maxRetries+1, lastErr)
}

func isPermanentAuthError(err error) bool {
	var apiErr *tgbotapi.Error
	if errors.As(err, &apiErr) {
		return apiErr.Code == 401 || apiErr.Code == 403
	}
	return false
}

// TokenSanitizer replaces bot token occurrences with "***" across strings,
// errors, URLs, and readers. Used by all HTTP exit paths for defense-in-depth
// token redaction.
type TokenSanitizer struct {
	token    string
	replacer *strings.Replacer
}

// NewTokenSanitizer creates a sanitizer that replaces token with "***".
func NewTokenSanitizer(token string) *TokenSanitizer {
	return &TokenSanitizer{
		token:    token,
		replacer: strings.NewReplacer(token, "***"),
	}
}

// Sanitize returns s with the token replaced by "***".
func (s *TokenSanitizer) Sanitize(text string) string {
	if s == nil || s.token == "" || !strings.Contains(text, s.token) {
		return text
	}
	return s.replacer.Replace(text)
}

// SanitizeError returns a new error with the token redacted.
// Returns nil if err is nil.
func (s *TokenSanitizer) SanitizeError(err error) error {
	if err == nil || s == nil || s.token == "" {
		return err
	}
	sanitized := s.replacer.Replace(err.Error())
	if sanitized == err.Error() {
		return err
	}
	return errors.New(sanitized)
}

// SanitizeURL returns a copy of u with the token segment in the path
// replaced by "***". The original URL is not modified.
func (s *TokenSanitizer) SanitizeURL(u *url.URL) *url.URL {
	if s == nil || s.token == "" || u == nil {
		return u
	}
	if !strings.Contains(u.String(), s.token) {
		return u
	}
	// Clone the URL and sanitize its string representation
	clone := *u
	sanitizedPath := s.replacer.Replace(clone.Path)
	clone.Path = sanitizedPath
	// Also sanitize RawPath if set (for URL-encoded paths)
	if clone.RawPath != "" {
		clone.RawPath = s.replacer.Replace(clone.RawPath)
	}
	return &clone
}

// sanitizingReadCloser wraps an io.ReadCloser and sanitizes reads.
type sanitizingReadCloser struct {
	rc       io.ReadCloser
	sanitizer *TokenSanitizer
	buf      []byte // 1 byte lookahead buffer for partial token detection at boundaries
}

// Read implements io.Reader with token sanitization.
// Uses a simple approach: buffers small reads to detect and replace token
// across read boundaries.
func (r *sanitizingReadCloser) Read(p []byte) (int, error) {
	// For simplicity, read into a larger buffer, sanitize, then copy.
	// We use a fixed 32KB buffer to avoid pathological cases.
	const bufSize = 32 * 1024
	tmp := make([]byte, len(p)+bufSize)
	n, err := r.rc.Read(tmp)
	if n > 0 {
		sanitized := r.sanitizer.Sanitize(string(tmp[:n]))
		copied := copy(p, sanitized)
		return copied, err
	}
	return n, err
}

func (r *sanitizingReadCloser) Close() error {
	return r.rc.Close()
}

// wrapReadCloser wraps an io.ReadCloser with token sanitization on read.
func (s *TokenSanitizer) WrapReadCloser(rc io.ReadCloser) io.ReadCloser {
	if s == nil || s.token == "" {
		return rc
	}
	return &sanitizingReadCloser{rc: rc, sanitizer: s}
}

// tokenRedactingTransport wraps an http.RoundTripper and redacts the bot
// token from error messages, request URLs (in errors), and optionally from
// response bodies.
type tokenRedactingTransport struct {
	inner     http.RoundTripper
	sanitizer *TokenSanitizer
}

func (t *tokenRedactingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.inner.RoundTrip(req)
	if err != nil {
		return resp, t.sanitizer.SanitizeError(err)
	}
	// Sanitize response body if the response indicates an error (non-2xx).
	if resp.StatusCode >= 400 {
		resp.Body = t.sanitizer.WrapReadCloser(resp.Body)
	}
	return resp, err
}
