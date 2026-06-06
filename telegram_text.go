package main

import (
	"log"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const multiPartSendGap = 80 * time.Millisecond

// Telegram Bot API: max length of a single text message (UTF-8 code points / runes).
const telegramMaxMessageRunes = 4096

// Hard cap on streamed reply size before finalize (OOM guard).
const maxFinalizeBytes = 512 << 10

// splitTelegramText splits s into chunks of at most maxRunes, preferring line breaks.
func splitTelegramText(s string, maxRunes int) []string {
	if maxRunes <= 0 {
		maxRunes = telegramMaxMessageRunes
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return []string{s}
	}
	var parts []string
	for start := 0; start < len(runes); {
		remain := len(runes) - start
		if remain <= maxRunes {
			parts = append(parts, string(runes[start:]))
			break
		}
		end := start + maxRunes
		window := runes[start:end]
		cut := len(window)
		// Prefer breaking at newline in the last 25% of the chunk.
		searchFrom := cut * 3 / 4
		breakAt := -1
		for i := cut - 1; i >= searchFrom; i-- {
			if window[i] == '\n' {
				breakAt = i
				break
			}
		}
		if breakAt > 0 {
			parts = append(parts, string(window[:breakAt]))
			start += breakAt + 1
			continue
		}
		parts = append(parts, string(window))
		start = end
	}
	return parts
}

// telegramPreviewTail returns the last maxRunes of text for draft preview (no cut marker).
func telegramPreviewTail(text string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = telegramMaxMessageRunes
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[len(runes)-maxRunes:])
}

// logPreview shortens text for logs (no user-visible [cut] marker).
func logPreview(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	return trimUTF8Bytes(s, maxBytes) + "…"
}

// trimUTF8Bytes trims s to at most maxBytes without breaking a UTF-8 code point.
func trimUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// truncateForButton ensures text fits Telegram's 64-byte inline keyboard button text limit.
func truncateForButton(text string) string {
	const maxBtnBytes = 64
	if len(text) <= maxBtnBytes {
		return text
	}
	return trimUTF8Bytes(text, maxBtnBytes-3) + "…"
}

func telegramErrorIsNotModified(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "message is not modified")
}

// sendTextParts delivers text as one or more Telegram messages (≤4096 runes each).
// Text is automatically converted from markdown to Telegram HTML format.
// If editFirstMsgID != nil and *editFirstMsgID > 0, the first part updates that message.
func (a *App) sendTextParts(chatID int64, text string, editFirstMsgID *int) int {
	text = formatForTelegram(text)
	parts := splitTelegramText(text, telegramMaxMessageRunes)
	if len(parts) == 0 {
		return 0
	}
	sent := 0
	for i, part := range parts {
		if i == 0 && editFirstMsgID != nil && *editFirstMsgID != 0 {
			edit := tgbotapi.NewEditMessageText(chatID, *editFirstMsgID, part)
			edit.ParseMode = "HTML"
			if _, err := a.bot.Send(edit); err != nil {
				if telegramErrorIsNotModified(err) {
					return 1
				}
				log.Printf("chat=%d: edit part 1/%d failed: %v", chatID, len(parts), err)
				return sent
			}
			sent++
			continue
		}
		msg := tgbotapi.NewMessage(chatID, part)
		msg.ParseMode = "HTML"
		m, err := a.bot.Send(msg)
		if err != nil {
			log.Printf("chat=%d: send part %d/%d failed: %v", chatID, i+1, len(parts), err)
			return sent
		}
		sent++
		if i == 0 && editFirstMsgID != nil && *editFirstMsgID == 0 {
			*editFirstMsgID = m.MessageID
		}
		if i+1 < len(parts) {
			time.Sleep(multiPartSendGap)
		}
	}
	return sent
}