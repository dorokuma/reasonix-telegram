package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

var seenMsgs sync.Map // inbound message_id dedup

func (a *App) handleMessage(m *tgbotapi.Message) {
	defer a.msgWg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC recovered: %v\nstack: %s", r, debug.Stack())
		}
	}()

	if m.From == nil {
		return
	}
	// Control commands (/stop, /cancel, /restart) must always be processed — skip rate limit.
	ctrlText := strings.TrimSpace(m.Text)
	if ctrlText == "" {
		ctrlText = strings.TrimSpace(m.Caption)
	}
	if !(ctrlText == "/stop" || ctrlText == "/cancel" || ctrlText == "/restart" || strings.HasPrefix(ctrlText, "/stop ") || strings.HasPrefix(ctrlText, "/cancel ")) {
		// Per-chat rate limit: at most 1 message per 3 seconds.
		const minInterval = 3 * time.Second
		now := time.Now()
		if last, loaded := a.rateLimits.LoadOrStore("msg:"+strconv.FormatInt(m.Chat.ID, 10), now); loaded {
			lastTime, ok := last.(time.Time)
			if !ok || now.Sub(lastTime) < minInterval {
				return // silently drop
			}
			a.rateLimits.Store("msg:"+strconv.FormatInt(m.Chat.ID, 10), now)
		}
	}
	// Inbound message_id dedup: drop duplicate Telegram updates.
	// LoadOrStore provides an atomic check-and-store, eliminating the TOCTOU
	// race between Load and Store. Entries older than 10 minutes are replaced
	// with the current timestamp (treated as new).
	if m.MessageID != 0 {
		now := time.Now().Unix()
		if actual, loaded := seenMsgs.LoadOrStore(m.MessageID, now); loaded {
			if ts, ok := actual.(int64); ok && now-ts < 600 {
				log.Printf("chat=%d: duplicate message %d ignored", m.Chat.ID, m.MessageID)
				return
			}
			// Stale entry (>10 min) — update timestamp and continue
			seenMsgs.Store(m.MessageID, now)
		}
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
	// Read text or caption from the incoming message.
	// Telegram puts media descriptions in Caption, not Text.
	const maxInputBytes = 32768
	text := m.Text
	if text == "" {
		text = m.Caption
	}
	if len(text) > maxInputBytes {
		log.Printf("chat=%d: truncating message from %d to %d bytes", m.Chat.ID, len(text), maxInputBytes)
		text = text[:maxInputBytes]
	}
	text = strings.TrimSpace(text)

	// Media group aggregation: batch consecutive photos sharing a media_group_id.
	if a.enqueueMediaGroup(m) {
		// Photo was enqueued into an album batch — the timer will flush and
		// call runTask with the aggregated prompt. Don't process individually.
		return
	}

	// Handle non-batched media (photo, document, video, GIF, voice, audio)
	// and stickers. Build a prompt fragment describing the content.
	var parts strings.Builder
	var mediaDataURLs []string
	if mr := a.handleIncomingMedia(m); mr.Text != "" {
		parts.WriteString(mr.Text)
		log.Printf("chat=%d: mediaResult.Text len=%d, DataURLs len=%d", m.Chat.ID, len(mr.Text), len(mr.DataURLs))
		if len(mr.DataURLs) > 0 {
			mediaDataURLs = mr.DataURLs
		}
	}
	if sp := a.handleSticker(m); sp != "" {
		if parts.Len() > 0 {
			parts.WriteString("\n")
		}
		parts.WriteString(sp)
	}

	// Embed multimodal data URLs directly in the text using a marker that
	// the reasonix agent will parse and convert to image Parts. This avoids
	// changing the HTTP API between bridge and reasonix serve.
	if len(mediaDataURLs) > 0 {
		log.Printf("chat=%d: embedding %d data URLs (capped at 10)", m.Chat.ID, len(mediaDataURLs))
		const maxImages = 10
		excess := 0
		if len(mediaDataURLs) > maxImages {
			excess = len(mediaDataURLs) - maxImages
			mediaDataURLs = mediaDataURLs[:maxImages]
		}
		if parts.Len() > 0 {
			// Insert media markers after the description but before user text
			mediaBlock := "\n"
			for _, du := range mediaDataURLs {
				mediaBlock += "[REASONIX_IMAGE:" + du + "]\n"
			}
			parts.WriteString(mediaBlock)
		} else {
			for _, du := range mediaDataURLs {
				parts.WriteString("[REASONIX_IMAGE:" + du + "]\n")
			}
		}
		if excess > 0 {
			parts.WriteString(fmt.Sprintf("\n[还有 %d 张图片未显示]", excess))
		}
	}

	if parts.Len() > 0 {
		prelude := strings.TrimSpace(parts.String())
		if text != "" {
			text = prelude + "\n" + text
		} else {
			text = prelude
		}
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return
	}

	// Check if we're awaiting a clarify answer.
	s := a.getOrCreateSession(m.Chat.ID)

	// Process clarify under lock, snapshot data needed outside the lock.
	var (
		clarifyHandled   bool
		submitPC         *clarifyState
		clarifyPrevMsgID int
		nextQText        string
		nextOptions      []string
		nextClarifyID    string
		nextHeader       string
	)
	func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		pc := s.pendingClarify
		log.Printf("chat=%d: handleMessage text=%q pendingClarify=%v", m.Chat.ID, "[message]", pc != nil)
		if pc == nil {
			return
		}

		cleanText := strings.TrimSpace(text)
		if cleanText == "/cancel" || cleanText == "/stop" || strings.HasPrefix(cleanText, "/cancel ") || strings.HasPrefix(cleanText, "/stop ") {
			s.pendingClarify = nil
			return // falls through to normal processing
		}

		// Store text answer (under lock to avoid concurrent-map write).
		answerText := text
		if pc.awaitingCustom {
			pc.awaitingCustom = false
			answerText = "(自定义) " + text
		}
		log.Printf("chat=%d: clarify text answer for q=%s: %s", m.Chat.ID, pc.questionID, logPreview(answerText, 100))
		pc.answers[pc.questionID] = []string{answerText}

		nextIdx := pc.qIndex + 1
		if nextIdx < len(pc.allQuestions) {
			// Advance to next question (all fields mutated under lock).
			nextQ := pc.allQuestions[nextIdx]
			pc.qIndex = nextIdx
			pc.questionID = nextQ.ID
			pc.choices = nextQ.Options
			var cidBytes [8]byte
			if _, err := rand.Read(cidBytes[:]); err == nil {
				pc.clarifyID = base64.RawURLEncoding.EncodeToString(cidBytes[:])
			} else {
				pc.clarifyID = strconv.FormatUint(atomic.AddUint64(&a.clarifySeq, 1), 36) // fallback
			}
			// Snapshot data needed outside lock.
			qText := escapeMdv2(strings.TrimSpace(nextQ.Text))
			if qText == "" {
				qText = escapeMdv2(strings.TrimSpace(nextQ.ID))
			}
			if qText == "" {
				qText = "请选择："
			}
			nextQText = qText
			nextOptions = nextQ.Options
			nextClarifyID = pc.clarifyID
			clarifyPrevMsgID = pc.messageID
			nextHeader = fmt.Sprintf("问题 %d/%d\n", nextIdx+1, len(pc.allQuestions))
			clarifyHandled = true
			return
		}

		// All answered — submit.
		clarifyPrevMsgID = pc.messageID
		submitPC = pc
		s.pendingClarify = nil
		clarifyHandled = true
	}()

	if !clarifyHandled {
		// No pending clarify (or was cancelled): continue to slash commands.
	} else if submitPC != nil {
		a.removeKeyboard(m.Chat.ID, clarifyPrevMsgID)
		a.submitClarifyAnswers(submitPC, m.Chat.ID)
		return
	} else {
		// Send next clarification question.
		a.removeKeyboard(m.Chat.ID, clarifyPrevMsgID)
		replyText := "❓ " + nextHeader + nextQText
		msg := newMessage(m.Chat.ID, replyText)
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		if len(nextOptions) > 0 {
			var rows [][]tgbotapi.InlineKeyboardButton
			for i, choice := range nextOptions {
				payload := fmt.Sprintf("%s%s:%d", prefixClarify, nextClarifyID, i)
				data := signCallback(s.hmacKey, payload)
				btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
				rows = append(rows, []tgbotapi.InlineKeyboardButton{
					{Text: btnText, CallbackData: &data},
				})
			}
			otherPayload := fmt.Sprintf("%s%s:%s", prefixClarify, nextClarifyID, actionOther)
			otherData := signCallback(s.hmacKey, otherPayload)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
			})
			msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
		}
		if _, err := a.sendWithRetry(msg, m.Chat.ID); err != nil {
			log.Printf("send failed: %v", err)
		}
		return
	}

	// Slash commands.
	switch {
	case text == "/start" || text == "/help":
		a.reply(m.Chat.ID, strings.Join([]string{
			"**🤖 Reasonix Telegram**",
			fmt.Sprintf("模式：%s", a.modeLabel()),
			"**常用指令**",
			"/stop — 中止当前回复",
			"/new — 新对话",
			"/model — 切换模型",
			"/effort — 推理深度",
			"**定时任务**",
			"/cron [分] [时] [日] [月] [周] [Prompt] — 创建定时任务",
			"/cron_list — 查看定时任务列表",
			"/cron_del [ID] — 删除定时任务",
			"**状态查看**",
			"/status — 当前状态",
			"/health — 服务状态",
			"/sessions — 活跃会话",
			"**模式切换**",
			"/chat — 聊天模式",
			"/code — 编程模式",
			"**其他**",
			"/restart — 重启桥接",
			fmt.Sprintf("缓冲 %d 字节 · 超时 %d 分钟", a.cfg.MaxOutputBytes, a.cfg.MaxDuration),
		}, "\n\n"))
		return

	case strings.HasPrefix(text, "/cron"):
		if text == "/cron" || text == "/cron@"+a.bot.Self.UserName {
			a.reply(m.Chat.ID, "用法: /cron [分] [时] [日] [月] [周] [Prompt]")
			return
		}
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/cron"), "@"+a.bot.Self.UserName))
		a.handleCron(m, args)
		return

	case strings.HasPrefix(text, "/cron_list"):
		a.handleCronList(m)
		return

	case strings.HasPrefix(text, "/cron_del"):
		if text == "/cron_del" || text == "/cron_del@"+a.bot.Self.UserName {
			a.reply(m.Chat.ID, "用法: /cron_del [任务ID]")
			return
		}
		args := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/cron_del"), "@"+a.bot.Self.UserName))
		a.handleCronDel(m, args)
		return

	case strings.HasPrefix(text, "/stop") || strings.HasPrefix(text, "/cancel"):
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		t := s.task
		pa := s.pendingApproval
		s.mu.Unlock()
		if t == nil {
			if pa == nil {
				a.reply(m.Chat.ID, "当前没有进行中的回复")
				return
			}
			// task 已丢失（如被 releaseTask 误清）但仍有 pending approval →
			// 直接通知 serve 取消，并清空 approval 状态。
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = a.postJSON(ctx, pa.port, "/cancel", map[string]any{})
			s.mu.Lock()
			s.pendingApproval = nil
			s.mu.Unlock()
			a.reply(m.Chat.ID, "🛑 已发送中止信号")
			return
		}
		// 先清空 pendingApproval，让 releaseTask 正常执行清理
		s.mu.Lock()
		s.pendingApproval = nil
		s.mu.Unlock()
		// Signal the goroutine: it will SIGINT the process group, wait up to
		// 5s, then SIGKILL if still alive. s.task is cleared by runTask's defer
		// when the process actually exits, NOT here — so /status stays accurate.
		t.StopTyping()
		t.cancel()
		a.reply(m.Chat.ID, "🛑 已发送中止信号")
		return

	case text == "/status":
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		busy := s.task != nil
		sessPort := s.servePort
		sessModel := s.model
		s.mu.Unlock()
		modelName := sessModel
		if modelName == "" {
			modelName = reasonixDefaultModel
		}
		if name, ok := modelByID(modelName); ok {
			modelName = name
		} else if i := strings.LastIndex(modelName, "/"); i >= 0 {
			if name, ok := modelByID(modelName[i+1:]); ok {
				modelName = name
			}
		}
		stateCN := "空闲"
		if busy {
			stateCN = "生成中"
		}
		lines := []string{
			fmt.Sprintf("**状态** %s", stateCN),
			fmt.Sprintf("**模式** %s", a.modeLabel()),
			fmt.Sprintf("**模型** %s", modelName),
		}
		// Usage stats — cumulative session totals.
		s.mu.Lock()
		cumPrompt := s.cumPrompt
		cumCompletion := s.cumCompletion
		cumTotal := s.cumTotal
		cumCost := s.cumCost
		s.mu.Unlock()

		// Session-cumulative cache from serve /status API (survives serve restart).
		sessHit, sessMiss := a.fetchServeCache(sessPort)
		sessTotal := sessHit + sessMiss

		if cumTotal > 0 || sessTotal > 0 {
			if cumTotal > 0 {
				lines = append(lines, fmt.Sprintf("**输入** %d 词元", cumPrompt))
				lines = append(lines, fmt.Sprintf("**输出** %d 词元", cumCompletion))
				lines = append(lines, fmt.Sprintf("**总量** %d 词元", cumTotal))
			}
			if sessTotal > 0 {
				hitRate := float64(sessHit) / float64(sessTotal) * 100
				lines = append(lines, fmt.Sprintf("**缓存** %.2f%%", hitRate))
			}
			if cumCost > 0 {
				lines = append(lines, fmt.Sprintf("**花费** %.4f 元", cumCost))
			}
		}
		// Context usage from serve.
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port > 0 {
			if used, window := fetchContext(port); window > 0 {
				pct := int(int64(used) * 100 / int64(window))
				threshold := 80 // default compact ratio
				left := threshold - pct
				if left < 0 {
					left = 0
				}
				shortUsed := shortTokens(used)
				shortWindow := shortTokens(window)
				if left > 0 {
					lines = append(lines, fmt.Sprintf("**上下文** %s / %s（%d%%）· %d%%后压缩", shortUsed, shortWindow, pct, left))
				} else {
					lines = append(lines, fmt.Sprintf("**上下文** %s / %s（%d%%）· 即将压缩", shortUsed, shortWindow, pct))
				}
			}
		}
		a.reply(m.Chat.ID, strings.Join(lines, "\n\n"))
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

	case text == "/verbose":
		go a.subagentDisplayHandler(m, "verbose")
		return
	case text == "/summary":
		go a.subagentDisplayHandler(m, "summary")
		return
	case text == "/silent":
		go a.subagentDisplayHandler(m, "silent")
		return

	default:
		if strings.HasPrefix(text, "/") {
			a.reply(m.Chat.ID, fmt.Sprintf("未知指令：%s\n发送 /help 查看可用指令", text))
			return
		}
	}

	// If this is a reply, include the original message as context.
	// Read from Text first, fall back to Caption, then local cache.
	if m.ReplyToMessage != nil {
		quote := m.ReplyToMessage.Text
		if quote == "" {
			quote = m.ReplyToMessage.Caption
		}
		// sendRichMessage omits .Text; try our local cache.
		if quote == "" {
			quote = a.lookupSentText(m.ReplyToMessage.MessageID)
			if quote != "" {
				log.Printf("chat=%d: reply quote from cache msgID=%d len=%d", m.Chat.ID, m.ReplyToMessage.MessageID, len(quote))
			} else {
				log.Printf("chat=%d: reply cache miss msgID=%d — trying forward fallback", m.Chat.ID, m.ReplyToMessage.MessageID)
			quote = a.fetchMessageText(m.Chat.ID, m.ReplyToMessage.MessageID)
			}
		}
		log.Printf("chat=%d: reply quote len=%d textPreview=%q captionPreview=%q fromBot=%v fromUser=%d",
			m.Chat.ID, len(quote), "[content]",
			"[content]",
			m.ReplyToMessage.From != nil && m.ReplyToMessage.From.IsBot,
			m.ReplyToMessage.From.ID)
		if quote != "" {
			text = fmt.Sprintf("[回复消息: %s]\n%s", quote, text)
		}
	} else {
		log.Printf("chat=%d: no ReplyToMessage (msgID=%d)", m.Chat.ID, m.MessageID)
	}

	a.runTask(m.Chat.ID, m.MessageID, text)
}



func (a *App) allowed(u *tgbotapi.User) bool {
	if len(a.cfg.AllowedUsers) == 0 {
		log.Printf("WARNING: ALLOWED_USERS not set, denying access to user %d.", u.ID)
		return false
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
	// Generate 32-byte HMAC key for callback signing.
	hmacKey := make([]byte, 32)
	if _, err := rand.Read(hmacKey); err == nil {
		s.hmacKey = hmacKey
	} else {
		log.Printf("chat=%d: failed to generate hmac key: %v", chatID, err)
	}
	a.sess[chatID] = s
	return s
}

func (a *App) resetReasonixSession(chatID int64) {
	a.stopServe(chatID)
	s := a.getOrCreateSession(chatID)

	// 等待旧 serve 的 encrypt goroutine 完成
	if s.encryptDone != nil {
		select {
		case <-s.encryptDone:
		case <-time.After(3 * time.Second):
			log.Printf("chat=%d: timed out waiting for encrypt goroutine", chatID)
		}
	}

	basePath := a.state.sessionPathForChat(chatID)
	plainPath := a.state.sessionPathForChatPlain(chatID)
	ckptDir := filepath.Join(filepath.Dir(basePath), fmt.Sprintf("%d.ckpt", chatID))

	// 删除所有会话数据
	for _, p := range []string{
		basePath,       // .jsonl.enc
		plainPath,      // .jsonl
		basePath + ".meta",
		plainPath + ".cost",
	} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			log.Printf("chat=%d: remove %s: %v", chatID, p, err)
		}
	}
	os.RemoveAll(ckptDir)

	// 重置 session 对象
	s.mu.Lock()
	s.sessionPath = ""
	s.cumPrompt = 0
	s.cumCompletion = 0
	s.cumTotal = 0
	s.cumCost = 0
	s.cumCurrency = ""
	s.lastUsage = wireUsage{}
	s.hmacKey = nil
	s.mu.Unlock()

	// 从 state.json 移除（检查错误，但不要因为文件不存在就报错）
	if err := a.state.remove(chatID); err != nil {
		log.Printf("chat=%d: state.remove failed: %v", chatID, err)
	}
}

func (a *App) reply(chatID int64, text string) {
	if n := a.sendTextParts(chatID, text, nil); n == 0 {
		log.Printf("chat=%d: send reply failed (empty)", chatID)
	}
}

// sendTyping shows the client "typing…" indicator (API returns bool, not Message).
// Uses a 5 second timeout to avoid accumulating hanging requests when Telegram API
// is slow — the typing indicator is best-effort and should never block the caller.
func (a *App) sendTyping(chatID int64) {
	done := make(chan struct{}, 1)
	go func() {
		if _, err := a.bot.Request(tgbotapi.NewChatAction(chatID, tgbotapi.ChatTyping)); err != nil {
			log.Printf("chat=%d: sendChatAction typing: %v", chatID, err)
		}
		done <- struct{}{}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		log.Printf("chat=%d: sendChatAction typing timed out (5s)", chatID)
	}
}

// beginTyping shows Telegram "typing…" until the returned stop function runs.
func (a *App) beginTyping(parentCtx context.Context, chatID int64) (stop func()) {
	ctx, cancel := context.WithCancel(parentCtx)
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
