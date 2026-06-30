package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func (a *App) healthHandler(m *tgbotapi.Message) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	lines := []string{fmt.Sprintf("模式: %s", a.modeLabel())}
	s, ok := a.sess[m.Chat.ID]
	if !ok {
		lines = append(lines, "当前聊天无活跃会话")
	} else {
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
		lines = append(lines, status)
	}
	a.reply(m.Chat.ID, strings.Join(lines, "\n"))
}

func (a *App) modeHandler(m *tgbotapi.Message, arg string) {
	// Block during restart, same as handleMessage.
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	var newMode string
	switch arg {
	case "chat", "":
		newMode = ModeChat
	case "code", "tool":
		newMode = ModeTool
	default:
		a.reply(m.Chat.ID, "用法：/chat 或 /code")
		return
	}
	if a.getMode() == newMode {
		a.reply(m.Chat.ID, fmt.Sprintf("当前已经是%s", modeLabelFor(newMode)))
		return
	}
	// Stop existing serve, switch mode, rewrite toml, restart.
	a.stopServe(m.Chat.ID)
	a.setMode(newMode)
	_ = a.ensureUserRulesLinked()
	if err := a.startServe(m.Chat.ID, true); err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("切换模式失败: %v", err))
		return
	}
	a.reply(m.Chat.ID, fmt.Sprintf("已切换到%s", modeLabelFor(newMode)))
}

func (a *App) modelHandler(m *tgbotapi.Message, arg string) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		if len(availableModels) == 0 {
			a.reply(m.Chat.ID, "⚠️ 未能加载可用模型列表。请检查 reasonix 配置。")
			return
		}
		a.sendModelPicker(m.Chat.ID, 0)
		return
	}

	name, ok := modelByID(arg)
	if !ok {
		if len(availableModels) == 0 {
			a.reply(m.Chat.ID, fmt.Sprintf("未知模型：%s\n⚠️ 未能加载可用模型列表。请检查 reasonix 配置。", arg))
			return
		}
		ids := make([]string, len(availableModels))
		for i, m := range availableModels {
			ids[i] = m.ID
		}
		a.reply(m.Chat.ID, fmt.Sprintf("未知模型：%s\n可用：%s", arg, strings.Join(ids, ", ")))
		return
	}

	s := a.getOrCreateSession(m.Chat.ID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = reasonixDefaultModel
	}
	s.mu.Unlock()

	if arg == current {
		a.reply(m.Chat.ID, fmt.Sprintf("当前已经是 %s", name))
		return
	}

	a.switchModel(m.Chat.ID, arg, name)
}

const modelsPerPage = 4

func (a *App) sendModelPicker(chatID int64, page int) {
	total := len(availableModels)
	totalPages := (total + modelsPerPage - 1) / modelsPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * modelsPerPage
	end := start + modelsPerPage
	if end > total {
		end = total
	}

	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = reasonixDefaultModel
	}
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		m := availableModels[i]
		label := m.Name
		if m.ID == current {
			label = "✅ " + label
		}
		payload := fmt.Sprintf("%s%s", prefixModel, m.ID)
		data := signCallback(s.hmacKey, payload)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: label, CallbackData: &data},
		})
	}

	// Pagination row
	if totalPages > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			pPayload := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page-1)
			pdata := signCallback(s.hmacKey, pPayload)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "◀️ 上一页", CallbackData: &pdata})
		}
		nav = append(nav, tgbotapi.InlineKeyboardButton{Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: strPtr("_")})
		if page < totalPages-1 {
			pPayload := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page+1)
			pdata := signCallback(s.hmacKey, pPayload)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "下一页 ▶️", CallbackData: &pdata})
		}
		rows = append(rows, nav)
	}

	text := fmt.Sprintf("🤖 选择模型（当前：%s）", a.modelDisplayName(current))
	msg := newMessage(chatID, escapeMdv2(text))
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendSafe(msg)
}

// editModelPicker edits an existing picker message with a new page of models.
func (a *App) editModelPicker(chatID int64, messageID int, page int) {
	total := len(availableModels)
	totalPages := (total + modelsPerPage - 1) / modelsPerPage
	if page < 0 {
		page = 0
	}
	if page >= totalPages {
		page = totalPages - 1
	}

	start := page * modelsPerPage
	end := start + modelsPerPage
	if end > total {
		end = total
	}

	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	current := s.model
	if current == "" {
		current = reasonixDefaultModel
	}
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for i := start; i < end; i++ {
		m := availableModels[i]
		label := m.Name
		if m.ID == current {
			label = "✅ " + label
		}
		payload := fmt.Sprintf("%s%s", prefixModel, m.ID)
		data := signCallback(s.hmacKey, payload)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: label, CallbackData: &data},
		})
	}

	// Pagination row
	if totalPages > 1 {
		var nav []tgbotapi.InlineKeyboardButton
		if page > 0 {
			pPayload := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page-1)
			pdata := signCallback(s.hmacKey, pPayload)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "◀️ 上一页", CallbackData: &pdata})
		}
		nav = append(nav, tgbotapi.InlineKeyboardButton{Text: fmt.Sprintf("%d/%d", page+1, totalPages), CallbackData: strPtr("_")})
		if page < totalPages-1 {
			pPayload := fmt.Sprintf("%s%s:%d", prefixModel, actionPage, page+1)
			pdata := signCallback(s.hmacKey, pPayload)
			nav = append(nav, tgbotapi.InlineKeyboardButton{Text: "下一页 ▶️", CallbackData: &pdata})
		}
		rows = append(rows, nav)
	}

	text := fmt.Sprintf("🤖 选择模型（当前：%s）", a.modelDisplayName(current))
	edit := tgbotapi.NewEditMessageText(chatID, messageID, escapeMdv2(text))
	edit.ParseMode = tgbotapi.ModeMarkdownV2
	edit.ReplyMarkup = &tgbotapi.InlineKeyboardMarkup{InlineKeyboard: rows}
	if _, err := a.bot.Request(edit); err != nil {
		log.Printf("edit model picker failed: %v", err)
		// Fallback: send a new message when editing the picker fails
		fallback := tgbotapi.NewMessage(chatID, fmt.Sprintf("✅ 模型已切换为 %s", a.modelDisplayName(current)))
		if _, sendErr := a.bot.Send(fallback); sendErr != nil {
			log.Printf("send fallback message failed: %v", sendErr)
		}
	}
}

func strPtr(s string) *string { return &s }

func (a *App) modelDisplayName(id string) string {
	if name, ok := modelByID(id); ok {
		return name
	}
	return id
}

// fetchContext queries the reasonix serve /context endpoint for used/window tokens.
func fetchContext(port int) (used, window int) {
	resp, err := localHTTPClient.Get(serveBaseURL(port) + "/context")
	if err != nil {
		return 0, 0
	}
	defer resp.Body.Close()
	var c struct {
		Used   int `json:"used"`
		Window int `json:"window"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return 0, 0
	}
	return c.Used, c.Window
}

// shortTokens formats a token count as 1.2K, 142.0K, 1.0M, etc.
func shortTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return strconv.Itoa(n)
	}
}

func (a *App) switchModel(chatID int64, modelID, modelName string) {
	s := a.getOrCreateSession(chatID)
	a.stopServe(chatID)
	s.mu.Lock()
	s.model = modelID
	s.mu.Unlock()
	// Persist to state.json.
	if err := a.persistModel(chatID, modelID); err != nil {
		log.Printf("chat=%d: persist model failed: %v", chatID, err)
	}
	if err := a.startServe(chatID, true); err != nil {
		a.reply(chatID, fmt.Sprintf("切换模型失败: %v", err))
		return
	}
	a.reply(chatID, fmt.Sprintf("已切换到 %s", modelName))
}

var effortLevels = []struct {
	ID   string
	Name string
}{
	{"auto", "自动 (默认)"},
	{"low", "低"},
	{"medium", "中"},
	{"high", "高"},
	{"max", "最高"},
}

func (a *App) effortHandler(m *tgbotapi.Message, arg string) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(m.Chat.ID, "🔄 桥接重启中，稍后再试。")
		return
	}
	a.restartMu.Unlock()

	arg = strings.ToLower(strings.TrimSpace(arg))
	if arg == "" {
		// Ensure serve is running so we have a port.
		s := a.getOrCreateSession(m.Chat.ID)
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port == 0 {
			if err := a.startServe(m.Chat.ID, true); err != nil {
				a.reply(m.Chat.ID, fmt.Sprintf("启动 serve 失败: %v", err))
				return
			}
		}
		a.sendEffortPicker(m.Chat.ID, 0)
		return
	}

	// Validate level.
	valid := false
	for _, l := range effortLevels {
		if l.ID == arg {
			valid = true
			break
		}
	}
	if !valid {
		a.reply(m.Chat.ID, "用法：/effort auto|low|medium|high|max")
		return
	}

	// Submit to reasonix serve.
	s := a.getOrCreateSession(m.Chat.ID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()
	if port == 0 {
		a.reply(m.Chat.ID, "serve 未运行")
		return
	}
	reqCtx, reqCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer reqCancel()
	if err := a.postJSON(reqCtx, port, "/submit", map[string]string{"input": "/effort " + arg}); err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("切换 effort 失败: %v", err))
		return
	}
	a.reply(m.Chat.ID, fmt.Sprintf("推理深度已切换到 %s", arg))
}

func (a *App) sendEffortPicker(chatID int64, _ int) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	port := s.servePort
	s.mu.Unlock()

	var rows [][]tgbotapi.InlineKeyboardButton
	for _, l := range effortLevels {
		payload := fmt.Sprintf("%s%s", prefixEffort, l.ID)
		data := signCallback(s.hmacKey, payload)
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			{Text: l.Name, CallbackData: &data},
		})
	}
	msg := newMessage(chatID, "🤔 选择推理深度")
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
	a.sendSafe(msg)
	_ = port // keep port reference for future use
}

// persistModel writes the model ID to state.json so it survives restarts.
func (a *App) persistModel(chatID int64, modelID string) error {
	return a.state.updateAll(func(records []chatRecord) []chatRecord {
		for i, rec := range records {
			if rec.ChatID == chatID {
				records[i].Model = modelID
				return records
			}
		}
		return append(records, chatRecord{ChatID: chatID, Model: modelID})
	})
}

func (a *App) subagentDisplayHandler(m *tgbotapi.Message, mode string) {
	chatID := m.Chat.ID
	s := a.getOrCreateSession(chatID)

	s.mu.Lock()
	s.subagentDisplay = mode
	s.mu.Unlock()

	// Persist to state.json.
	a.persistSubagentDisplay(chatID, mode)

	// Short confirmation message.
	label := map[string]string{
		"verbose": "详细模式",
		"summary": "摘要模式",
		"silent":  "静默模式",
	}[mode]

	text := "✅ " + label
	msg := tgbotapi.NewMessage(chatID, text)
	a.sendWithRetry(msg, chatID)
}

func (a *App) persistSubagentDisplay(chatID int64, mode string) {
	_ = a.state.updateAll(func(records []chatRecord) []chatRecord {
		for i, rec := range records {
			if rec.ChatID == chatID {
				records[i].SubagentDisplay = mode
				return records
			}
		}
		return append(records, chatRecord{ChatID: chatID, SubagentDisplay: mode})
	})
}

// signCallback signs a callback payload with HMAC-SHA256 and returns
// "payload.base64url(signature)".  If key is nil (legacy), returns payload unchanged.
func signCallback(key []byte, payload string) string {
	if key == nil {
		return payload
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

// verifyCallback verifies a signed callback string.  It extracts the payload and
// HMAC-SHA256 signature, checks the MAC, and returns the verified payload on
// success.  If key is nil (legacy) it returns the raw input unchanged.
func verifyCallback(key []byte, signed string) (string, bool) {
	if key == nil {
		return signed, true
	}
	idx := strings.LastIndex(signed, ".")
	if idx < 0 {
		return "", false
	}
	payload := signed[:idx]
	sig, err := base64.RawURLEncoding.DecodeString(signed[idx+1:])
	if err != nil {
		return "", false
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	if !hmac.Equal(expected, sig) {
		return "", false
	}
	return payload, true
}

func (a *App) handleCallbackQuery(cq *tgbotapi.CallbackQuery) {
	defer a.msgWg.Done()
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC recovered: %v\nstack: %s", r, debug.Stack())
		}
	}()

	if cq.Message == nil || cq.From == nil {
		return
	}
	chatID := cq.Message.Chat.ID

	// Per-chat rate limit: at most 1 callback per 3 seconds.
	const minInterval = 3 * time.Second
	now := time.Now()
	if last, loaded := a.rateLimits.LoadOrStore("cb:"+strconv.FormatInt(chatID, 10), now); loaded {
		if now.Sub(last.(time.Time)) < minInterval {
			return // silently drop
		}
		a.rateLimits.Store("cb:"+strconv.FormatInt(chatID, 10), now)
	}
	if !a.allowed(cq.From) {
		a.answerCallback(cq.ID, "⛔ 无权限")
		return
	}
	data := strings.TrimSpace(cq.Data)
	log.Printf("chat=%d: callback data=%q", chatID, logPreview(data, 200))

	// Verify HMAC-SHA256 signature on the callback data.
	s := a.getOrCreateSession(chatID)
	verifiedPayload, ok := verifyCallback(s.hmacKey, data)
	if !ok {
		log.Printf("chat=%d: HMAC verification failed for callback %q", chatID, logPreview(data, 200))
		a.answerCallback(cq.ID, "⛔ 验证失败")
		return
	}
	log.Printf("chat=%d: verified payload=%q", chatID, logPreview(verifiedPayload, 200))
	data = verifiedPayload // use verified payload for all downstream processing

	// --- Approval callbacks: ap:{approvalID}:{action} ---
	if strings.HasPrefix(data, prefixApproval) {
		parts := strings.SplitN(data, ":", 3)
		if len(parts) < 3 {
			return
		}
		approvalID := parts[1]
		action := parts[2]

		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		pa := s.pendingApproval
		if pa == nil || pa.approvalID != approvalID {
			s.mu.Unlock()
			return
		}
		s.pendingApproval = nil
		s.mu.Unlock()

		a.answerCallback(cq.ID, "")
		a.removeKeyboard(chatID, cq.Message.MessageID)

		var allow bool
		var session bool
		switch action {
		case actionOnce:
			allow = true
			session = false
		case actionSession:
			allow = true
			session = true
		default: // deny
			allow = false
			session = false
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		body := map[string]any{
			"id": approvalID, "allow": allow,
		}
		if pa.scope != "task" {
			body["session"] = session
		}
		if err := a.postJSON(ctx, pa.port, "/approve", body); err != nil {
			a.reply(chatID, "操作失败，reasonix serve 可能已重启，请重新操作")
		}
		log.Printf("chat=%d: approval %s -> allow=%v session=%v", chatID, approvalID, allow, session)
		return
	}

	// --- Model callbacks: md:{modelID} or md:page:{page} ---
	if strings.HasPrefix(data, prefixModel) {
		payload := strings.TrimPrefix(data, prefixModel)
		if payload == "_" {
			a.answerCallback(cq.ID, "")
			return // no-op (page indicator)
		}
		if strings.HasPrefix(payload, actionPage+":") {
			// Pagination — edit current message instead of sending a new one
			a.answerCallback(cq.ID, "")
			pageStr := strings.TrimPrefix(payload, actionPage+":")
			page, _ := strconv.Atoi(pageStr)
			a.editModelPicker(chatID, cq.Message.MessageID, page)
			return
		}
		// Model selection
		modelID := payload
		name, ok := modelByID(modelID)
		if !ok {
			a.answerCallback(cq.ID, "⚠️ 模型列表已更新，请重新选择")
			a.sendModelPicker(chatID, 0)
			return
		}
		a.answerCallback(cq.ID, "")
		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		current := s.model
		if current == "" {
			current = reasonixDefaultModel
		}
		s.mu.Unlock()
		// Delete the picker message entirely — bubble + buttons both gone.
		a.deleteMessage(chatID, cq.Message.MessageID)
		if modelID == current {
			a.reply(chatID, fmt.Sprintf("当前已经是 %s", name))
			return
		}
		a.switchModel(chatID, modelID, name)
		return
	}

	// --- Effort callbacks: ef:{level} ---
	if strings.HasPrefix(data, prefixEffort) {
		a.answerCallback(cq.ID, "")
		level := strings.TrimPrefix(data, prefixEffort)
		if level == "" {
			return
		}
		// Validate level against known effort levels.
		valid := false
		for _, l := range effortLevels {
			if l.ID == level {
				valid = true
				break
			}
		}
		if !valid {
			return
		}

		// Get serve port from session.
		s := a.getOrCreateSession(chatID)
		s.mu.Lock()
		port := s.servePort
		s.mu.Unlock()
		if port == 0 {
			a.reply(chatID, "serve 未运行")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := a.postJSON(ctx, port, "/submit", map[string]string{"input": "/effort " + level}); err != nil {
			a.reply(chatID, fmt.Sprintf("切换 effort 失败: %v", err))
			return
		}
		a.removeKeyboard(chatID, cq.Message.MessageID)
		a.reply(chatID, fmt.Sprintf("推理深度已切换到 %s", level))
		return
	}

	// --- Clarify callbacks: cl:{clarifyID}:{choice} ---
	if !strings.HasPrefix(data, prefixClarify) {
		return
	}
	parts := strings.SplitN(data, ":", 3)
	if len(parts) < 3 {
		return
	}
	clarifyID := parts[1]
	choiceIdx := parts[2]

	s.mu.Lock()
	pc := s.pendingClarify
	if pc == nil || pc.clarifyID != clarifyID {
		s.mu.Unlock()
		return
	}

	if choiceIdx == actionOther {
		log.Printf("chat=%d: clarify 'other' clicked, pendingClarify=%v", chatID, pc != nil)
		pc.awaitingCustom = true
		msgID := pc.messageID
		s.mu.Unlock()
		a.answerCallback(cq.ID, "")
		a.removeKeyboard(chatID, msgID)
		msg := newMessage(chatID, "📝 请输入你的回答：")
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		msg.ReplyToMessageID = cq.Message.MessageID
		a.sendSafe(msg)
		return
	}

	// Resolve button choice to answer text.
	pc.awaitingCustom = false
	var answer string
	if idx, err := strconv.Atoi(choiceIdx); err == nil && idx >= 0 && idx < len(pc.choices) {
		answer = pc.choices[idx]
	}
	if answer == "" {
		answer = choiceIdx
	}

	// Store answer and advance (all under lock to avoid concurrent-map write).
	pc.answers[pc.questionID] = []string{answer}
	nextIdx := pc.qIndex + 1
	if nextIdx < len(pc.allQuestions) {
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
		// Snapshot data for message building.
		qText := escapeMdv2(strings.TrimSpace(nextQ.Text))
		if qText == "" {
			qText = escapeMdv2(strings.TrimSpace(nextQ.ID))
		}
		if qText == "" {
			qText = "请选择："
		}
		options := nextQ.Options
		clarifyID := pc.clarifyID
		prevMsgID := pc.messageID
		s.mu.Unlock()

		a.removeKeyboard(chatID, prevMsgID)

		header := fmt.Sprintf("问题 %d/%d\n", nextIdx+1, len(pc.allQuestions))
		text := "❓ " + header + qText
		msg := newMessage(chatID, text)
		msg.ParseMode = tgbotapi.ModeMarkdownV2
		if len(options) > 0 {
			var rows [][]tgbotapi.InlineKeyboardButton
			for i, choice := range options {
				payload := fmt.Sprintf("%s%s:%d", prefixClarify, clarifyID, i)
				data := signCallback(s.hmacKey, payload)
				btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
				rows = append(rows, []tgbotapi.InlineKeyboardButton{
					{Text: btnText, CallbackData: &data},
				})
			}
			otherPayload := fmt.Sprintf("%s%s:%s", prefixClarify, clarifyID, actionOther)
			otherData := signCallback(s.hmacKey, otherPayload)
			rows = append(rows, []tgbotapi.InlineKeyboardButton{
				{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
			})
			msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
		}
		if _, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("send failed: %v", err)
		}
		a.answerCallback(cq.ID, "")
		return
	}

	// All questions answered — submit.
	prevMsgID := pc.messageID
	s.pendingClarify = nil
	s.mu.Unlock()
	a.removeKeyboard(chatID, prevMsgID)

	a.answerCallback(cq.ID, "")
	a.submitClarifyAnswers(pc, chatID)
}

func (a *App) formatChoices(choices []string) string {
	var b strings.Builder
	for i, c := range choices {
		if i > 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "%d. %s", i+1, escapeMdv2(c))
	}
	return b.String()
}

func (a *App) sessionsHandler(m *tgbotapi.Message) {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	s, ok := a.sess[m.Chat.ID]
	if !ok {
		a.reply(m.Chat.ID, "当前聊天无活跃会话")
		return
	}
	s.mu.Lock()
	la := s.lastActivity
	s.mu.Unlock()
	line := fmt.Sprintf("聊天 %d", m.Chat.ID)
	if !la.IsZero() {
		line += fmt.Sprintf(" · 最后活跃 %s 前", time.Since(la).Round(time.Second))
	}
	a.reply(m.Chat.ID, line)
}
