package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
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

	innerTransport := &tokenRedactingTransport{
		inner: http.DefaultTransport,
		token: cfg.BotToken,
	}
	bot.Client = &http.Client{
		Transport: NewTelegramFallbackTransport(innerTransport, os.Getenv("TELEGRAM_FALLBACK_IPS")),
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
