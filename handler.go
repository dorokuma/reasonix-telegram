package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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
	// Read text or caption from the incoming message.
	// Telegram puts media descriptions in Caption, not Text.
	text := m.Text
	if text == "" {
		text = m.Caption
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
	parts := ""
	if mp := a.handleIncomingMedia(m); mp != "" {
		parts = mp
	}
	if sp := a.handleSticker(m); sp != "" {
		if parts != "" {
			parts += "\n" + sp
		} else {
			parts = sp
		}
	}
	if parts != "" {
		if text != "" {
			text = parts + "\n" + text
		} else {
			text = parts
		}
	}

	text = strings.TrimSpace(text)
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
			"**🤖 Reasonix Telegram**",
			fmt.Sprintf("模式：%s", a.modeLabel()),
			"**常用指令**",
			"/stop — 中止当前回复",
			"/new — 新对话",
			"/model — 切换模型",
			"/effort — 推理深度",
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
				lines = append(lines, fmt.Sprintf("**花费** %.2f 元", cumCost))
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
				log.Printf("chat=%d: reply cache miss msgID=%d", m.Chat.ID, m.ReplyToMessage.MessageID)
			}
		}
		log.Printf("chat=%d: reply quote len=%d textPreview=%q captionPreview=%q fromBot=%v fromUser=%d",
			m.Chat.ID, len(quote), logPreview(m.ReplyToMessage.Text, 60),
			logPreview(m.ReplyToMessage.Caption, 60),
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
