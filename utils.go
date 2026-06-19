package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// killDescendants terminates a process tree with SIGTERM + grace + SIGKILL.
// Best-effort: relies on /proc being readable, which is true on Linux.
func killDescendants(rootPid int) {
	visited := map[int]bool{rootPid: true}
	// Phase 1: SIGTERM all descendants
	var walkTerm func(int)
	walkTerm = func(ppid int) {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pid, err := strconv.Atoi(e.Name())
			if err != nil || visited[pid] {
				continue
			}
			statBytes, err := os.ReadFile("/proc/" + e.Name() + "/stat")
			if err != nil {
				continue
			}
			stat := string(statBytes)
			rpar := strings.LastIndex(stat, ")")
			if rpar < 0 || rpar+2 >= len(stat) {
				continue
			}
			fields := strings.Fields(stat[rpar+2:])
			if len(fields) < 2 {
				continue
			}
			parent, err := strconv.Atoi(fields[1])
			if err != nil {
				continue
			}
			if parent == ppid {
				visited[pid] = true
				_ = syscall.Kill(pid, syscall.SIGTERM)
				walkTerm(pid)
			}
		}
	}
	walkTerm(rootPid)

	// Phase 2: wait for graceful exit (up to 3s)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		allDone := true
		for pid := range visited {
			if pid == rootPid {
				continue
			}
			if err := syscall.Kill(pid, 0); err == nil {
				allDone = false
				break
			}
		}
		if allDone {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Phase 3: SIGKILL survivors
	for pid := range visited {
		if pid == rootPid {
			continue
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}


// sendSafe sends a message with retry and logs any error.
func (a *App) sendSafe(msg tgbotapi.Chattable) {
	if _, err := a.sendWithRetry(msg, 0); err != nil {
		log.Printf("send failed: %v", err)
	}
}

// answerCallback answers a callback query and logs any error.
func (a *App) answerCallback(id, text string) {
	if _, err := a.bot.Request(tgbotapi.NewCallback(id, text)); err != nil {
		log.Printf("callback answer failed: %v", err)
	}
}

// removeKeyboard removes the inline keyboard from a message (call after user selects).
func (a *App) removeKeyboard(chatID int64, messageID int) {
	edit := tgbotapi.NewEditMessageReplyMarkup(chatID, messageID, tgbotapi.InlineKeyboardMarkup{
		InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{},
	})
	if _, err := a.bot.Request(edit); err != nil && !telegramErrorIsNotModified(err) {
		log.Printf("remove keyboard failed: %v", err)
	}
}

// deleteMessage deletes a message entirely.
func (a *App) deleteMessage(chatID int64, messageID int) {
	if _, err := a.bot.Request(tgbotapi.NewDeleteMessage(chatID, messageID)); err != nil {
		log.Printf("delete message failed: %v", err)
	}
}

// editCommentary updates a tool-dispatch message.  Renders markdown via
// MarkdownV2 (Hermes pattern: formatMessage → parse → plain fallback).
// On edit failure, deletes the old message and resends — never gives up.
// Returns the new message ID when a resend occurred (0 if edit succeeded in-place).
func (a *App) editCommentary(chatID int64, messageID int, appendText string) (int, error) {
	text := capTelegramMessage(appendText)
	// MDV2 path: formatMessage converts standard Markdown → MarkdownV2.
	formatted := formatMessage(text)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, formatted)
	edit.ParseMode = tgbotapi.ModeMarkdownV2
	_, err := a.sendWithRetry(edit, chatID)
	if telegramEditOK(err) {
		return 0, nil
	}
	// MDV2 parse failure — strip formatting and retry as plain text.
	if telegramErrorIsParseEntities(err) {
		plain := stripMdv2(text)
		edit2 := tgbotapi.NewEditMessageText(chatID, messageID, plain)
		edit2.ParseMode = ""
		if _, err2 := a.sendWithRetry(edit2, chatID); telegramEditOK(err2) {
			return 0, nil
		}
	}
	// Edit failed — delete + resend with MDV2.
	log.Printf("chat=%d: edit commentary failed (%v), delete+resend", chatID, err)
	a.deleteMessage(chatID, messageID)
	msg := newMessage(chatID, formatted)
	msg.ParseMode = tgbotapi.ModeMarkdownV2
	sent, sendErr := a.sendWithRetry(msg, chatID)
	if sendErr != nil {
		// MDV2 resend failed — retry as plain text.
		plain := stripMdv2(text)
		msg2 := newMessage(chatID, plain)
		msg2.ParseMode = ""
		sent, sendErr2 := a.sendWithRetry(msg2, chatID)
		if sendErr2 != nil {
			log.Printf("chat=%d: commentary resend failed: %v", chatID, sendErr2)
			return 0, sendErr2
		}
		return sent.MessageID, nil
	}
	return sent.MessageID, nil
}

// appendToCommentary replaces a tool-dispatch message (same caps as editCommentary).
func (a *App) appendToCommentary(chatID int64, messageID int, appendText string) (int, error) {
	return a.editCommentary(chatID, messageID, appendText)
}

// submitClarifyAnswers POSTs accumulated clarify answers to the serve backend.
func (a *App) submitClarifyAnswers(pc *clarifyState, chatID int64) {
	type answerEntry struct {
		QuestionID string   `json:"questionId"`
		Selected   []string `json:"selected"`
	}
	var answersPayload []answerEntry
	for _, q := range pc.allQuestions {
		sel, ok := pc.answers[q.ID]
		if !ok {
			sel = []string{""}
		}
		answersPayload = append(answersPayload, answerEntry{
			QuestionID: q.ID,
			Selected:   sel,
		})
	}
	body, _ := json.Marshal(map[string]any{
		"id":      pc.askID,
		"answers": answersPayload,
	})
	url := fmt.Sprintf("http://127.0.0.1:%d/answer", pc.port)
	resp, err := localHTTPClient.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		log.Printf("chat=%d: post answer failed: %v", chatID, err)
		return
	}
	resp.Body.Close()
	log.Printf("chat=%d: all %d answers submitted to serve (askID=%s)", chatID, len(pc.allQuestions), pc.askID)

	// Kick the turn pusher: model will continue emitting events on the existing SSE stream.
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.wakePusher != nil {
		s.wakePusher()
	}
	s.mu.Unlock()
}

// userFacingError maps known Reasonix errors to short Chinese for Telegram users.
func userFacingError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "paused after"):
		return "本轮已暂停（达到步数上限），可再发一条消息继续"
	case strings.Contains(s, "reasonix serve not ready"):
		return "Reasonix 未就绪，请稍后重试或发送 /restart"
	case strings.HasPrefix(s, "submit:"):
		return "提交失败：" + strings.TrimSpace(strings.TrimPrefix(s, "submit:"))
	case strings.Contains(s, "connection refused"):
		return "无法连接 Reasonix 服务"
	default:
		return s
	}
}

// recordSentText stores the text of a sent message by its ID for later
// reply/quote extraction. sendRichMessage omits .Text from the Telegram
// message object, so ReplyToMessage.Text is empty when users reply.
func (a *App) recordSentText(msgID int, text string) {
	// Cap at 500 entries to avoid unbounded growth.
	count := 0
	a.sentTextCache.Range(func(_, _ any) bool { count++; return count < 500 })
	if count >= 500 {
		// Evict oldest entries (approximate: clear half).
		n := 0
		a.sentTextCache.Range(func(k, _ any) bool {
			if n < 250 {
				a.sentTextCache.Delete(k)
				n++
			}
			return true
		})
	}
	a.sentTextCache.Store(msgID, text)
	a.saveSentTextCache()
}

// lookupSentText retrieves the stored text for a message ID, or "" if not found.
func (a *App) lookupSentText(msgID int) string {
	if v, ok := a.sentTextCache.Load(msgID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// saveSentTextCache writes the entire in-memory cache to disk as JSON.
// Uses atomic write (tmp file + rename) to prevent partial-file corruption.
// Called after every recordSentText. The file is capped at ~500 entries so
// the write is small (~50 KB) and does not need throttling.
func (a *App) saveSentTextCache() {
	if a.sentTextCachePath == "" {
		return
	}
	m := make(map[int]string, 500)
	a.sentTextCache.Range(func(k, v any) bool {
		if id, ok := k.(int); ok {
			if s, ok := v.(string); ok {
				m[id] = s
			}
		}
		return true
	})
	b, err := json.Marshal(m)
	if err != nil {
		log.Printf("sentTextCache: marshal: %v", err)
		return
	}

	a.sentTextCacheMu.Lock()
	defer a.sentTextCacheMu.Unlock()

	// Atomic write: tmp file → rename to prevent partial file on crash.
	tmpPath := a.sentTextCachePath + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0o600); err != nil {
		log.Printf("sentTextCache: write tmp %s: %v", tmpPath, err)
		return
	}
	if err := os.Rename(tmpPath, a.sentTextCachePath); err != nil {
		log.Printf("sentTextCache: rename %s → %s: %v", tmpPath, a.sentTextCachePath, err)
	}
}

// loadSentTextCache reads the disk cache into the in-memory sync.Map.
// Called once at startup. Errors are logged but not fatal — a missing or
// corrupt cache file simply means we start with an empty cache.
func (a *App) loadSentTextCache() {
	if a.sentTextCachePath == "" {
		return
	}
	disk := loadSentTextCacheFromDisk(a.sentTextCachePath)
	for id, text := range disk {
		a.sentTextCache.Store(id, text)
	}
	log.Printf("sentTextCache: loaded %d entries from %s", len(disk), a.sentTextCachePath)
}

// loadSentTextCacheFromDisk reads and parses the JSON cache file.
// Returns nil if the file does not exist or is corrupt.
func loadSentTextCacheFromDisk(path string) map[int]string {
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("sentTextCache: read %s: %v", path, err)
		}
		return nil
	}
	var m map[int]string
	if err := json.Unmarshal(b, &m); err != nil {
		log.Printf("sentTextCache: unmarshal %s: %v", path, err)
		return nil
	}
	return m
}

// fetchMessageText retrieves the text of a message by temporarily forwarding
// it and immediately deleting the copy. This is a last-resort fallback when
// ReplyToMessage.Text is empty (sendRichMessage) and the local cache misses
// (e.g. messages sent before the persistent cache was deployed).
// The forward is silent (no notification) and the copy is deleted within the
// same handler turn, so users should not notice it.
// The retrieved text is also recorded into the sentTextCache for future lookups.
func (a *App) fetchMessageText(chatID int64, msgID int) string {
	fwd := tgbotapi.NewForward(chatID, chatID, msgID)
	fwd.DisableNotification = true
	msg, err := a.bot.Send(fwd)
	if err != nil {
		log.Printf("chat=%d: fetchMessageText forward %d: %v", chatID, msgID, err)
		return ""
	}
	text := msg.Text
	if text == "" {
		text = msg.Caption
	}
	// Delete the forwarded copy immediately.
	a.deleteMessage(chatID, msg.MessageID)
	if text != "" {
		log.Printf("chat=%d: fetchMessageText msgID=%d len=%d", chatID, msgID, len(text))
		a.recordSentText(msgID, text)
	} else {
		text = "⚠️ 无法获取引用消息内容（bot 重启后旧消息的引用文本不可恢复）"
	}
	return text
}
