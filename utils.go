package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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

// editCommentary updates a tool-dispatch message. Caps length and treats
// "message is not modified" as success.
func (a *App) editCommentary(chatID int64, messageID int, appendText string) error {
	text := capTelegramMessage(appendText)
	edit := tgbotapi.NewEditMessageText(chatID, messageID, formatForTelegram(text))
	edit.ParseMode = "MarkdownV2"
	_, err := a.sendWithRetry(edit, chatID)
	if telegramEditOK(err) {
		return nil
	}
	log.Printf("chat=%d: edit commentary failed: %v", chatID, err)
	return err
}

// appendToCommentary replaces a tool-dispatch message (same caps as editCommentary).
func (a *App) appendToCommentary(chatID int64, messageID int, appendText string) error {
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
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
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
