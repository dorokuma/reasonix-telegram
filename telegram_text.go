package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const multiPartSendGap = 80 * time.Millisecond

// Telegram Bot API: max length of a single text message (UTF-8 code points / runes).
const telegramMaxMessageRunes = 4096

// telegramMaxFormattedRunes is the safe truncation limit for formatted text,
// reserving ~10% headroom for MarkdownV2 escaping expansion.
const telegramMaxFormattedRunes = 3700

// Hard cap on streamed reply size before finalize (OOM guard).
const maxFinalizeBytes = 512 << 10
const maxMediaSize = 50 << 20 // 50 MB max for media uploads
const telegramMaxCaptionRunes = 1024 // Telegram Bot API caption limit

// isMediaFilePath detects media file type from extension.
func isMediaFilePath(path string) (mediaType string, ok bool) {
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		ext := strings.ToLower(path[idx:])
		if qi := strings.Index(ext, "?"); qi >= 0 {
			ext = ext[:qi]
		}
		switch ext {
		case ".jpg", ".jpeg", ".png", ".webp", ".bmp":
			return "photo", true
		case ".gif":
			return "animation", true
		case ".mp4", ".webm", ".mov", ".avi", ".mkv":
			return "video", true
		case ".mp3", ".ogg", ".wav", ".flac", ".m4a":
			return "audio", true
		}
	}
	return "", false
}

// sendNativeMedia sends a local file as Telegram native media.
func (a *App) sendNativeMedia(chatID int64, path string, caption string) bool {
	mediaType, ok := isMediaFilePath(path)
	if !ok {
		return false
	}
	if fi, err := os.Stat(path); os.IsNotExist(err) || (err == nil && fi.Size() > maxMediaSize) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		log.Printf("chat=%d: open media %s: %v", chatID, path, err)
		return false
	}
	defer f.Close()
	file := tgbotapi.FileReader{Name: filepath.Base(path), Reader: f}
	switch mediaType {
	case "photo":
		msg := tgbotapi.NewPhoto(chatID, file)
		if caption != "" {
			msg.Caption = caption
		}
		if _, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("chat=%d: sendPhoto %s: %v", chatID, path, err)
			return false
		}
		return true
	case "video":
		msg := tgbotapi.NewVideo(chatID, file)
		if caption != "" {
			msg.Caption = caption
		}
		if _, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("chat=%d: sendVideo %s: %v", chatID, path, err)
			return false
		}
		return true
	case "animation":
		msg := tgbotapi.NewAnimation(chatID, file)
		if caption != "" {
			msg.Caption = caption
		}
		if _, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("chat=%d: sendAnimation %s: %v", chatID, path, err)
			return false
		}
		return true
	case "audio":
		msg := tgbotapi.NewAudio(chatID, file)
		if caption != "" {
			msg.Caption = caption
		}
		if _, err := a.sendWithRetry(msg, chatID); err != nil {
			log.Printf("chat=%d: sendAudio %s: %v", chatID, path, err)
			return false
		}
		return true
	}
	return false
}

// sendDocument sends a file as native Telegram document.
func (a *App) sendDocument(chatID int64, path string, caption string) bool {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return false
	}
	if fi, err := os.Stat(path); os.IsNotExist(err) || (err == nil && fi.Size() > maxMediaSize) {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		log.Printf("chat=%d: open document %s: %v", chatID, path, err)
		return false
	}
	defer f.Close()
	msg := tgbotapi.NewDocument(chatID, tgbotapi.FileReader{Name: filepath.Base(path), Reader: f})
	if caption != "" {
		msg.Caption = caption
	}
	if _, err := a.sendWithRetry(msg, chatID); err != nil {
		log.Printf("chat=%d: sendDocument %s: %v", chatID, path, err)
		return false
	}
	return true
}

// extractAndSendMedia scans text for file paths and sends matching files as native media.
// Only files under the workdir or /tmp are sent to prevent information disclosure.
func (a *App) extractAndSendMedia(chatID int64, text string) string {
	lines := strings.Split(text, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		path := ""
		if strings.HasPrefix(trimmed, "file://") {
			path = strings.TrimPrefix(trimmed, "file://")
		} else if strings.HasPrefix(trimmed, "/") && len(trimmed) > 5 && !strings.Contains(trimmed, " ") {
			path = trimmed
		}
		if path != "" && a.isSafeMediaPath(path) {
			if a.sendNativeMedia(chatID, path, "") {
				continue
			}
			if a.sendDocument(chatID, path, "") {
				continue
			}
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// isSafeMediaPath checks if a file path is safe to send as a Telegram attachment.
// Only files under the workdir or /tmp are allowed, preventing information disclosure
// of sensitive files like /etc/passwd, session data, or API keys.
// Resolves symlinks to prevent bypass via symlinked paths.
func (a *App) isSafeMediaPath(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	// Resolve symlinks to get the real target.
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		// If resolution fails (broken symlink, permission denied), treat as unsafe.
		return false
	}
	workdir := a.chatWorkdir()
	if workdir != "" {
		rel, err := filepath.Rel(workdir, resolved)
		if err == nil && !strings.HasPrefix(rel, "..") && rel != ".." {
			return true
		}
	}
	if strings.HasPrefix(resolved, "/tmp/") {
		return true
	}
	return false
}

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
			parts = append(parts, string(window[:breakAt+1]))
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
	if len(s) <= maxBytes {
		return fmt.Sprintf("[%d bytes] %q", len(s), s)
	}
	truncated := trimUTF8Bytes(s, maxBytes)
	return fmt.Sprintf("[%d bytes] %q...", len(s), truncated)
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

// capTelegramMessage trims text to fit one Telegram message, reserving ~10%
// headroom for MarkdownV2 escaping expansion (leaves 3700 of the 4096 limit).
func capTelegramMessage(text string) string {
	const safeMaxRunes = 3700
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= safeMaxRunes {
		return text
	}
	const suffix = "\n…（已截断）"
	suffixRunes := len([]rune(suffix))
	if safeMaxRunes <= suffixRunes {
		return string(runes[:safeMaxRunes])
	}
	return string(runes[:safeMaxRunes-suffixRunes]) + suffix
}

// newMessage creates a MessageConfig with link preview disabled (Hermes parity).
func newMessage(chatID int64, text string) tgbotapi.MessageConfig {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	return msg
}

// sendTextParts delivers text as one or more Telegram messages (≤4096 runes each).
// Tries Telegram MarkdownV2 first; on entity-parse failure retries as plain text
// (with the MDV2 escape backslashes and formatting markers stripped via _stripMdv2,
// Hermes pattern). If editFirstMsgID != nil and *editFirstMsgID > 0, the first
// part updates that message.
func (a *App) sendTextParts(chatID int64, text string, editFirstMsgID *int, noFileFallback ...bool) (ret int) {
	defer func() {
		log.Printf("chat=%d: sendTextParts returns %d", chatID, ret)
	}()
	text = strings.TrimSpace(text)
	if text == "" {
		log.Printf("chat=%d: sendTextParts called with empty text, returning 0", chatID)
		return 0
	}
	// Check if file fallback is disabled (recursion guard)
	if len(noFileFallback) > 0 && noFileFallback[0] && len(text) > telegramMaxMessageRunes*3 {
		// Split long text into multiple parts without file fallback
		parts := splitTelegramText(text, telegramMaxMessageRunes)
		total := 0
		for i, p := range parts {
			msg := newMessage(chatID, p)
			if i > 0 {
				time.Sleep(multiPartSendGap)
			}
			if _, err := a.sendWithRetry(msg, chatID); err != nil {
				log.Printf("chat=%d: noFileFallback part %d/%d: %v", chatID, i+1, len(parts), err)
				break
			}
			total++
		}
		return total
	}
	// Extract and send native media before formatting text
	text = a.extractAndSendMedia(chatID, text)
	text = strings.TrimSpace(text)
	if text == "" {
		return 1
	}
	// Try Rich Messages with raw markdown.
	var editID int
	if editFirstMsgID != nil {
		editID = *editFirstMsgID
	}
	if msgID := a.tryRichMessage(chatID, text, editID); msgID > 0 {
		a.recordSentText(msgID, text)
		return 1
	}
	return a.sendFormattedParts(chatID, capTelegramMessage(text), editFirstMsgID, "")
}

func (a *App) sendFormattedParts(chatID int64, displayText string, editFirstMsgID *int, parseMode string) int {
	parts := splitTelegramText(displayText, telegramMaxMessageRunes)
	if len(parts) == 0 {
		log.Printf("chat=%d: sendFormattedParts: splitTelegramText returned 0 parts, text len=%d", chatID, len(displayText))
		return 0
	}

	// If no parseMode specified, use MDV2 with formatMessage (Hermes pattern).
	if parseMode == "" {
		parseMode = tgbotapi.ModeMarkdownV2
		// Format each part individually after split, so formatting markers
		// (e.g. ```fence```) are not broken across chunk boundaries.
		formatted := make([]string, len(parts))
		for i, part := range parts {
			formatted[i] = formatMessage(part)
		}
		parts = formatted
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
	_, err := a.sendWithRetry(edit, chatID)
	if !telegramEditOK(err) {
		if telegramErrorIsMessageTooLong(err) && len(parts) == 1 && len([]rune(parts[0])) > 500 {
			sub := splitTelegramText(parts[0], telegramMaxMessageRunes/2)
			if len(sub) > 1 {
				log.Printf("chat=%d: edit overflow reactive split (%d subchunks)", chatID, len(sub))
				return a.editOverflowSplit(chatID, messageID, sub, parseMode)
			}
		}
		if telegramErrorIsParseEntities(err) && parseMode == tgbotapi.ModeMarkdownV2 {
			// MDV2 parse failure — strip formatting and retry as plain text.
			plain := stripMdv2(parts[0])
			edit2 := tgbotapi.NewEditMessageText(chatID, messageID, plain)
			edit2.ParseMode = ""
			if _, err2 := a.sendWithRetry(edit2, chatID); telegramEditOK(err2) {
				if len(parts) == 1 {
					return 1
				}
				sent := 1
				replyTo := messageID
				sent += a.sendMessageParts(chatID, parts[1:], parseMode, replyTo)
				return sent
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
		msg := newMessage(chatID, part)
		if parseMode != "" {
			msg.ParseMode = parseMode
		}
		if replyTo != 0 {
			msg.ReplyToMessageID = replyTo
		}
		m, err := a.sendWithRetry(msg, chatID)
		if err != nil {
			if telegramErrorIsParseEntities(err) && parseMode == tgbotapi.ModeMarkdownV2 {
				// MDV2 parse failure — strip formatting and retry as plain text.
				plain := stripMdv2(part)
				msg.Text = plain
				msg.ParseMode = ""
				m, err = a.sendWithRetry(msg, chatID)
				if err != nil {
					log.Printf("chat=%d: send part %d/%d failed (even plain): %v", chatID, i+1, len(parts), err)
					return sent
				}
			} else if telegramErrorIsParseEntities(err) {
				return sent
			} else {
				log.Printf("chat=%d: send part %d/%d failed: %v", chatID, i+1, len(parts), err)
				return sent
			}
		}
		sent++
		replyTo = m.MessageID
		if i+1 < len(parts) {
			time.Sleep(multiPartSendGap)
		}
	}
	return sent
}

// sendWithRetry sends any Chattable with retry for flood/network errors.
func (a *App) sendWithRetry(msg tgbotapi.Chattable, chatID int64) (tgbotapi.Message, error) {
	const maxAttempts = 3
	reFloodWait := regexp.MustCompile(`(?i)retry after (\d+)`)

	// Dedup: skip duplicate text sends within 30s.
	contentKey := messageContentKey(msg)
	if contentKey != "" {
		a.sentCacheMu.Lock()
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte(contentKey)))
		if lastSent, ok := a.sentCache[hash]; ok && time.Since(lastSent) < 30*time.Second {
			a.sentCacheMu.Unlock()
			log.Printf("chat=%d: dedup — same content sent %v ago, skipping", chatID, time.Since(lastSent).Round(time.Second))
			return tgbotapi.Message{}, nil
		}

		// Clean entries older than 30s.
		now := time.Now()
		for h, t := range a.sentCache {
			if now.Sub(t) > 30*time.Second {
				delete(a.sentCache, h)
			}
		}
		a.sentCacheMu.Unlock()
	}

	var lastErr error
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var parseMode, displayText string
		switch v := msg.(type) {
		case tgbotapi.MessageConfig:
			parseMode = v.ParseMode
			displayText = v.Text
		case tgbotapi.EditMessageTextConfig:
			parseMode = v.ParseMode
			displayText = v.Text
		case tgbotapi.PhotoConfig:
			parseMode = v.ParseMode
			displayText = v.Caption
		case tgbotapi.VideoConfig:
			parseMode = v.ParseMode
			displayText = v.Caption
		case tgbotapi.AnimationConfig:
			parseMode = v.ParseMode
			displayText = v.Caption
		case tgbotapi.AudioConfig:
			parseMode = v.ParseMode
			displayText = v.Caption
		case tgbotapi.DocumentConfig:
			parseMode = v.ParseMode
			displayText = v.Caption
		default:
			displayText = fmt.Sprintf("%T", msg)
		}
		log.Printf("DEBUG SEND chat=%d parse_mode=%s text=[%s]", chatID, parseMode, displayText)
		log.Printf("DEBUG SEND chat=%d parse_mode=%s text=[%s]", chatID, parseMode, displayText)
		m, err := a.bot.Send(msg)
		if err == nil {
			// Record successful send in cache.
			if contentKey != "" {
				hash := fmt.Sprintf("%x", sha256.Sum256([]byte(contentKey)))
				a.sentCacheMu.Lock()
				a.sentCache[hash] = time.Now()
				a.sentCacheMu.Unlock()
			}
			return m, nil
		}
		lastErr = err
		if telegramErrorIsParseEntities(err) || telegramErrorIsNotModified(err) {
			return m, err
		}
		if telegramErrorIsFlood(err) {
			var waitSec int
			if match := reFloodWait.FindStringSubmatch(err.Error()); len(match) > 1 {
				if ws, err := strconv.Atoi(match[1]); err == nil && ws > 0 {
					waitSec = ws
				}
			}
			if waitSec < 1 {
				waitSec = 5
			}
			log.Printf("chat=%d: flood wait %ds (attempt %d/%d)", chatID, waitSec, attempt+1, maxAttempts)
			time.Sleep(time.Duration(waitSec) * time.Second)
			continue
		}
		if attempt+1 < maxAttempts {
			backoff := time.Duration(1<<uint(attempt)) * time.Second
			log.Printf("chat=%d: transient error, retry in %v (attempt %d/%d): %v", chatID, backoff, attempt+1, maxAttempts, err)
			time.Sleep(backoff)
			continue
		}
	}
	return tgbotapi.Message{}, lastErr
}

// messageContentKey returns a string uniquely identifying the content of a
// Chattable message for dedup hashing. Returns "" for types where dedup is
// not applicable (media, documents, etc.).
func messageContentKey(msg tgbotapi.Chattable) string {
	switch v := msg.(type) {
	case tgbotapi.MessageConfig:
		return "text:" + v.Text
	case tgbotapi.EditMessageTextConfig:
		return fmt.Sprintf("edit:%d:%s", v.MessageID, v.Text)
	default:
		// For other types (media, documents, etc.), use the full struct
		// representation as a conservative fallback.
		return fmt.Sprintf("%T:%+v", msg, msg)
	}
}

// tryRichMessage attempts to send text via sendRichMessage with raw markdown.
// If editMsgID > 0, it edits the existing message via editMessageText with rich_message.
// Returns the message ID on success, 0 on failure. For edits, returns editMsgID[0].
func (a *App) tryRichMessage(chatID int64, text string, editMsgID ...int) int {
	const maxLen = 32768
	runes := []rune(text)
	if len(runes) > maxLen {
		runes = runes[:maxLen]
	}
	msgText := string(runes)
	richMsg, err := marshalAPI(map[string]any{"markdown": msgText})
	if err != nil {
		log.Printf("chat=%d: marshal rich_message: %v", chatID, err)
		return 0
	}
	params := tgbotapi.Params{
		"rich_message": richMsg,
	}
	endpoint := "sendRichMessage"
	if len(editMsgID) > 0 && editMsgID[0] > 0 {
		endpoint = "editMessageText"
		params["chat_id"] = strconv.FormatInt(chatID, 10)
		params["message_id"] = strconv.FormatInt(int64(editMsgID[0]), 10)
		// editMessageText requires a text field even with rich_message.
		params["text"] = msgText
	} else {
		params["chat_id"] = strconv.FormatInt(chatID, 10)
	}
	resp, err := a.bot.MakeRequest(endpoint, params)
	if err != nil {
		log.Printf("chat=%d: %s failed: %v", chatID, endpoint, err)
		return 0
	}
	log.Printf("chat=%d: tryRichMessage succeeded", chatID)
	// For edits, the message ID is already known.
	if len(editMsgID) > 0 && editMsgID[0] > 0 {
		return editMsgID[0]
	}
	// Parse message ID from response.
	var msg struct {
		MessageID int `json:"message_id"`
	}
	if err := json.Unmarshal(resp.Result, &msg); err != nil {
		log.Printf("chat=%d: sendRichMessage: parse response: %v", chatID, err)
		return 1 // assume success, return 1 as fallback
	}
	// sendRichMessage already populates .Text from rich_message content
	// (confirmed by "message is not modified" on redundant edit).
	return msg.MessageID
}

// marshalAPI JSON-encodes v, returning an empty string on failure with the error.
func marshalAPI(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(b), nil
}