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

func telegramErrorIsMessageTooLong(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "MESSAGE_TOO_LONG")
}

func telegramErrorIsParseEntities(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "can't parse entities") ||
		strings.Contains(s, "cant parse entities") ||
		strings.Contains(s, "can't find end tag")
}

func telegramErrorIsFlood(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "retry after") ||
		strings.Contains(s, "flood") ||
		strings.Contains(s, "too many requests")
}

// telegramEditOK reports whether an edit failure can be treated as success.
func telegramEditOK(err error) bool {
	return err == nil || telegramErrorIsNotModified(err)
}

// streamContinuationText returns the portion of final not already visible in the
// streamed preview. When the preview was a tail slice, final does not start with
// visiblePrefix — return the full final so fallback send delivers the answer.
func streamContinuationText(final, visiblePrefix string) string {
	final = strings.TrimSpace(final)
	visiblePrefix = strings.TrimSpace(visiblePrefix)
	if final == "" {
		return ""
	}
	if visiblePrefix == "" {
		return final
	}
	if strings.HasPrefix(final, visiblePrefix) {
		return strings.TrimSpace(final[len(visiblePrefix):])
	}
	return final
}

// capTelegramMessage trims text to fit one Telegram message (≤4096 runes).
func capTelegramMessage(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= telegramMaxMessageRunes {
		return text
	}
	const suffix = "\n…（已截断）"
	suffixRunes := len([]rune(suffix))
	if telegramMaxMessageRunes <= suffixRunes {
		return string(runes[:telegramMaxMessageRunes])
	}
	return string(runes[:telegramMaxMessageRunes-suffixRunes]) + suffix
}

// sendTextParts delivers text as one or more Telegram messages (≤4096 runes each).
// Tries Telegram HTML first; on entity-parse failure retries as plain text (Hermes pattern).
// If editFirstMsgID != nil and *editFirstMsgID > 0, the first part updates that message.
func (a *App) sendTextParts(chatID int64, text string, editFirstMsgID *int) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}
	if n := a.sendFormattedParts(chatID, formatForTelegram(text), editFirstMsgID, "HTML"); n > 0 {
		return n
	}
	log.Printf("chat=%d: HTML delivery failed, retrying plain text (%d runes)", chatID, utf8.RuneCountInString(text))
	return a.sendFormattedParts(chatID, capTelegramMessage(text), editFirstMsgID, "")
}

func (a *App) sendFormattedParts(chatID int64, displayText string, editFirstMsgID *int, parseMode string) int {
	parts := splitTelegramText(displayText, telegramMaxMessageRunes)
	if len(parts) == 0 {
		return 0
	}
	if editFirstMsgID != nil && *editFirstMsgID != 0 {
		return a.editOverflowSplit(chatID, *editFirstMsgID, parts, parseMode)
	}
	return a.sendMessageParts(chatID, parts, parseMode, 0)
}

// editOverflowSplit edits the first chunk in-place, then sends continuations as
// reply-threaded messages (Hermes Telegram _edit_overflow_split, lightweight).
func (a *App) editOverflowSplit(chatID int64, messageID int, parts []string, parseMode string) int {
	if len(parts) == 0 {
		return 0
	}
	edit := tgbotapi.NewEditMessageText(chatID, messageID, parts[0])
	if parseMode != "" {
		edit.ParseMode = parseMode
	}
	_, err := a.bot.Send(edit)
	if !telegramEditOK(err) {
		if telegramErrorIsMessageTooLong(err) && len(parts) == 1 && len([]rune(parts[0])) > 500 {
			sub := splitTelegramText(parts[0], telegramMaxMessageRunes/2)
			if len(sub) > 1 {
				log.Printf("chat=%d: edit overflow reactive split (%d subchunks)", chatID, len(sub))
				return a.editOverflowSplit(chatID, messageID, sub, parseMode)
			}
		}
		if telegramErrorIsParseEntities(err) {
			return 0
		}
		log.Printf("chat=%d: edit part 1/%d failed: %v", chatID, len(parts), err)
		return 0
	}
	sent := 1
	replyTo := messageID
	if len(parts) == 1 {
		return sent
	}
	sent += a.sendMessageParts(chatID, parts[1:], parseMode, replyTo)
	return sent
}

func (a *App) sendMessageParts(chatID int64, parts []string, parseMode string, replyTo int) int {
	sent := 0
	for i, part := range parts {
		msg := tgbotapi.NewMessage(chatID, part)
		if parseMode != "" {
			msg.ParseMode = parseMode
		}
		if replyTo != 0 {
			msg.ReplyToMessageID = replyTo
		}
		m, err := a.bot.Send(msg)
		if err != nil {
			if telegramErrorIsParseEntities(err) {
				return sent
			}
			log.Printf("chat=%d: send part %d/%d failed: %v", chatID, i+1, len(parts), err)
			return sent
		}
		sent++
		replyTo = m.MessageID
		if i+1 < len(parts) {
			time.Sleep(multiPartSendGap)
		}
	}
	return sent
}