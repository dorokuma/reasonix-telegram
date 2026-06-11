package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// runTask streams the model reply into one Telegram bubble (sendMessageDraft when
// supported, else editMessageText). No "running/done" status prefix — only text.
func (a *App) runTask(chatID int64, replyTo int, prompt string) {
	s := a.getOrCreateSession(chatID)

	s.mu.Lock()
	s.lastActivity = time.Now()
	if t := s.task; t != nil {
		log.Printf("chat=%d: pre-empting running turn", chatID)
		t.cancel()
	}
	s.mu.Unlock()
	a.dismissSessionDraft(chatID)

	deadline := time.Now().Add(3 * time.Second)
	for {
		s.mu.Lock()
		busy := s.task != nil
		s.mu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("WARN: chat=%d previous turn didn't exit in 3s", chatID)
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := a.ensureServe(chatID); err != nil {
		a.reply(chatID, fmt.Sprintf("Reasonix 服务启动失败: %v", err))
		return
	}

	stopTyping := a.beginTyping(chatID)
	defer stopTyping()

	var ctx context.Context
	var cancel context.CancelFunc
	if a.cfg.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(a.cfg.MaxDuration)*time.Minute)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	s.mu.Lock()
	s.task = &runningTask{cancel: cancel}
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.task = nil
		s.wakePusher = nil
		s.mu.Unlock()
		cancel()
	}()

	var (
		buf            strings.Builder
		bufMu          sync.Mutex
		draftMu        sync.Mutex
		truncated      bool
		finished       = make(chan struct{})
		flushNow       = make(chan struct{}, 1) // reasonix "message" / turn_done — finalize early
		pushWake       = make(chan struct{}, 1)
		newSegment     = make(chan struct{}, 1) // tool boundary: finalize + reset
		streamMsgID    int
		draftID        = a.nextDraftID()
		useDraft       = true
		draftShown     bool // sendMessageDraft succeeded for current draftID
		liveDraftEver  bool // any sendMessageDraft succeeded this segment (survives state resets)
		streamDone     bool
		lastDraftBody  string
		msgCreatedAt   time.Time // when first draft/stream msg was sent
		draftFailCount       int  // consecutive draft failures in this turn
		editFailCount        int  // consecutive edit failures in this turn
		streamEditFallback   bool // edit flood-silenced: finalize via sendMessage tail
		streamVisiblePrefix  string // last raw preview successfully shown (edit/draft)
	)
	const (
		maxDraftFailures = 3
		maxEditFailures  = 3
		freshFinalAfter  = 30 * time.Second
		streamDebounce   = 50 * time.Millisecond
	)
	var procErr error
	replyDelivered := false
	releaseTask := func() {
		s.mu.Lock()
		s.task = nil
		s.wakePusher = nil
		s.mu.Unlock()
	}
	// endStream mirrors TelePi finalizeResponse: set streamDone first, flush last
	// draft frame, then sendMessage so no late sendMessageDraft lands after the real message.
	// Fresh final: if the first preview was sent >30s ago, create a new message
	// instead of editing the stale preview (so TG timestamp reflects completion time).
	endStream := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			return
		}
		if draftShown || liveDraftEver {
			a.clearDraftPreview(chatID, draftID)
			draftShown = false
			liveDraftEver = false
		}

		bufMu.Lock()
		raw := buf.String()
		tr := truncated
		bufMu.Unlock()
		body := streamFinalizeBody(raw, lastDraftBody)
		if body != "" && strings.TrimSpace(raw) == "" && strings.TrimSpace(lastDraftBody) != "" {
			log.Printf("chat=%d: endStream using lastDraftBody fallback len=%d", chatID, len(body))
		}
		log.Printf("chat=%d: endStream useDraft=%v draftID=%d bodyLen=%d bodyPreview=%q", chatID, useDraft, draftID, len(body), logPreview(body, 100))
		if body == "" {
			hadPreview := draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody)
			if hadPreview {
				a.clearDraftPreview(chatID, draftID)
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			streamDone = true
			releaseTask()
			log.Printf("chat=%d: endStream body empty, clearedDraft=%v useDraft=%v", chatID, hadPreview, useDraft)
			return
		}
		if len(body) > maxFinalizeBytes {
			body = trimUTF8Bytes(body, maxFinalizeBytes)
			tr = true
		}
		if tr {
			body += "\n\n（内容过长，已截断尾部）"
		}
		// Fresh final: if msgCreatedAt is set and old, send as new message
		// instead of editing the stale one.
		useFreshFinal := !msgCreatedAt.IsZero() && time.Since(msgCreatedAt) > freshFinalAfter
		var n int
		if useDraft && !useFreshFinal {
			n = a.finalizeDraft(chatID, draftID, body, draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody))
			if n > 0 {
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			log.Printf("chat=%d draftID=%d: finalize %d part(s) total=%d runes", chatID, draftID, n, utf8.RuneCountInString(body))
		} else {
			if useFreshFinal && streamMsgID > 0 {
				log.Printf("chat=%d: fresh final (stale preview >%ds), sending new message", chatID, int(freshFinalAfter.Seconds()))
			}
			hadLiveDraft := draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody)
			if streamMsgID > 0 && !useFreshFinal {
				if streamEditFallback {
					tail := streamContinuationText(body, streamVisiblePrefix)
					if tail == "" {
						n = 1
						log.Printf("chat=%d: finalize fallback skip (already shown)", chatID)
					} else {
						log.Printf("chat=%d: finalize fallback send continuation len=%d", chatID, len(tail))
						n = a.sendTextParts(chatID, tail, nil)
					}
				} else {
					streamed := formatForTelegram(telegramPreviewTail(body, telegramMaxMessageRunes))
					if streamed == formatForTelegram(lastDraftBody) {
						n = 1
						log.Printf("chat=%d: finalize skip edit (already shown via stream)", chatID)
					} else {
						editID := streamMsgID
						n = a.sendTextParts(chatID, body, &editID)
					}
				}
			} else {
				n = a.sendTextParts(chatID, body, nil)
			}
			if n > 0 && hadLiveDraft {
				a.clearDraftPreview(chatID, draftID)
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			if n == 0 {
				log.Printf("chat=%d: finalize send failed (0 parts), stream stays open for retry", chatID)
			}
		}
		if n > 0 {
			replyDelivered = true
			streamDone = true
			releaseTask()
		}
	}

	retireLiveDraftLocked := func(reason string) {
		if !draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
			return
		}
		a.clearDraftPreview(chatID, draftID)
		draftShown = false
		liveDraftEver = false
		lastDraftBody = ""
		log.Printf("chat=%d: retired live draft (%s) draftID=%d", chatID, reason, draftID)
	}

	pushDraft := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			log.Printf("chat=%d: pushDraft skip (streamDone)", chatID)
			return
		}
		bufMu.Lock()
		body := strings.TrimSpace(buf.String())
		bufMu.Unlock()
		if body == "" {
			log.Printf("chat=%d: pushDraft skip (empty buffer)", chatID)
			return
		}
		preview := telegramPreviewTail(body, telegramMaxMessageRunes)
		// Native drafts and edit-in-place must not mix: an open sendMessageDraft
		// blocks the user's input even when the preview is invisible. Once we have
		// a stream message to edit, stay on the edit path for this segment.
		if useDraft && streamMsgID == 0 {
			if preview == lastDraftBody {
				return
			}
			if a.sendDraft(chatID, draftID, preview) {
				draftFailCount = 0
				lastDraftBody = preview
				draftShown = true
				liveDraftEver = true
				a.trackSessionDraft(chatID, draftID)
				if msgCreatedAt.IsZero() {
					msgCreatedAt = time.Now()
				}
				return
			}
			draftFailCount++
			if draftFailCount >= maxDraftFailures {
				log.Printf("chat=%d: disabling draft stream after %d failures", chatID, draftFailCount)
			}
			retireLiveDraftLocked("draft_send_failed")
			useDraft = false
			draftID = a.nextDraftID()
		}
		if streamMsgID == 0 {
			previewHTML := formatForTelegram(preview)
			msg := newMessage(chatID, previewHTML)
			msg.ParseMode = "MarkdownV2"
			sent, err := a.sendWithRetry(msg, chatID)
			if err != nil {
				log.Printf("chat=%d: stream send failed: %v", chatID, err)
				editFailCount++
				return
			}
			editFailCount = 0
			streamMsgID = sent.MessageID
			lastDraftBody = preview
			streamVisiblePrefix = preview
			if msgCreatedAt.IsZero() {
				msgCreatedAt = time.Now()
			}
			return
		}
		if streamEditFallback {
			return
		}
		if preview == lastDraftBody {
			return
		}
		previewHTML := formatForTelegram(preview)
		edit := tgbotapi.NewEditMessageText(chatID, streamMsgID, previewHTML)
		edit.ParseMode = "MarkdownV2"
		_, err := a.sendWithRetry(edit, chatID)
		if telegramEditOK(err) {
			editFailCount = 0
			lastDraftBody = preview
			streamVisiblePrefix = preview
			return
		}
		editFailCount++
		if telegramErrorIsFlood(err) || editFailCount >= maxEditFailures {
			streamEditFallback = true
			streamVisiblePrefix = lastDraftBody
			log.Printf("chat=%d: stream edit fallback (flood=%v strikes=%d)", chatID, telegramErrorIsFlood(err), editFailCount)
		}
	}

	signalFlush := func() {
		// Acquire draftMu so this does not race with endStream/pushDraft
		// which also use draftMu to protect streamDone, draftShown, etc.
		draftMu.Lock()
		// Native sendMessageDraft holds the Telegram composer until dismissed — do not
		// wait for finalize/sendMessage; unblock the user as soon as the model finishes.
		a.dismissSessionDraft(chatID)
		draftMu.Unlock()
		select {
		case flushNow <- struct{}{}:
		default:
		}
	}

	wakePush := func() {
		select {
		case pushWake <- struct{}{}:
		default:
		}
	}

	// Register pusher signal on session so clarify answer handlers can kick the stream.
	s.mu.Lock()
	s.wakePusher = wakePush
	s.mu.Unlock()

	go func() {
		defer func() {
			// Belt-and-suspenders: ensure pusher sees a flush even if turn_done
			// onComplete was missed (SSE dropped before turn_done).
			signalFlush()
			close(finished)
		}()
		procErr = a.runServeTurn(ctx, chatID, prompt,
			func(chunk string) {
				bufMu.Lock()
				appendChunk(&buf, chunk, a.cfg.MaxOutputBytes, &truncated)
				bufMu.Unlock()
				wakePush()
			},
			signalFlush,
			func() {
				// onToolDispatch: finalize current text segment and start fresh
				select {
				case newSegment <- struct{}{}:
				default:
				}
			},
			func(text string) int {
				// onCommentary: send a standalone message (tool progress, result)
				// Not part of the stream buffer — send immediately as new message.
				// Don't touch draftMu to avoid contention with pusher goroutine.
				text = capTelegramMessage(text)
				msg := newMessage(chatID, formatForTelegram(text))
				msg.ParseMode = "MarkdownV2"
				sent, err := a.sendWithRetry(msg, chatID)
				if err != nil {
					log.Printf("chat=%d: commentary send failed: %v", chatID, err)
					return 0
				}
				replyDelivered = true
				return sent.MessageID
			},
			func(askID string, questions []askQuestionData) {
				// onAskRequest: model wants user input (ask tool).
				if len(questions) == 0 {
					return
				}

				// Reset stream state so post-answer output can flow in a fresh draft.
				draftMu.Lock()
				if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
					a.clearDraftPreview(chatID, draftID)
				}
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				bufMu.Unlock()
				draftID = a.nextDraftID()
				useDraft = true
				lastDraftBody = ""
				streamMsgID = 0
				msgCreatedAt = time.Now()
				draftMu.Unlock()

				// Build answers map and store all questions for multi-question tracking
				answers := make(map[string][]string, len(questions))

				// Show the FIRST question with buttons
				q := questions[0]
				cidNum := atomic.AddUint64(&a.clarifySeq, 1)
				cid := strconv.FormatUint(cidNum, 36)
				s.mu.Lock()
				s.pendingClarify = &clarifyState{
					question:      q.Text,
					choices:       q.Options,
					askID:         askID,
					questionID:    q.ID,
					port:          s.servePort,
					clarifyID:     cid,
					allQuestions:  questions,
					qIndex:        0,
					answers:       answers,
				}
				s.mu.Unlock()

				// Send question with header + question text + options with descriptions
				qText := _escapeMdv2(strings.TrimSpace(q.Text))
				if qText == "" {
					qText = _escapeMdv2(strings.TrimSpace(q.ID))
				}
				if qText == "" {
					qText = "请选择："
				}
				header := ""
				if len(questions) > 1 {
					header = fmt.Sprintf("问题 1/%d\n", len(questions))
				}
				text := "❓ " + header + qText
				msg := newMessage(chatID, text)
				msg.ParseMode = "MarkdownV2"
				if len(q.Options) > 0 {
					var rows [][]tgbotapi.InlineKeyboardButton
					for i, choice := range q.Options {
						data := fmt.Sprintf("%s%s:%d", prefixClarify, cid, i)
						btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
						rows = append(rows, []tgbotapi.InlineKeyboardButton{
							{Text: btnText, CallbackData: &data},
						})
					}
					otherData := fmt.Sprintf("%s%s:%s", prefixClarify, cid, actionOther)
					rows = append(rows, []tgbotapi.InlineKeyboardButton{
						{Text: "✏️ 其他（输入答案）", CallbackData: &otherData},
					})
					msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(rows...)
				}
				// Send message and store ID for keyboard removal
				if sent, err := a.sendWithRetry(msg, chatID); err != nil {
					log.Printf("send failed: %v", err)
				} else {
					s.mu.Lock()
					s.pendingClarify.messageID = sent.MessageID
					s.mu.Unlock()
				}
				replyDelivered = true
			},
			func(approvalID, toolName string) {
				// onApprovalRequest: model needs user approval for a tool.
				// Finalize current stream content first.
				signalFlush()
				// Reset stream state so post-approval output can flow in a fresh draft.
				draftMu.Lock()
				if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
					a.clearDraftPreview(chatID, draftID)
				}
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				bufMu.Unlock()
				draftID = a.nextDraftID()
				useDraft = true
				lastDraftBody = ""
				streamMsgID = 0
				msgCreatedAt = time.Now()
				draftMu.Unlock()
				replyDelivered = true

				// Show approval prompt with inline buttons
				var label string
				var emoji string
				switch toolName {
				default:
					label = toolName
					emoji = "🔧"
				}

				// Set pendingApproval for callback
				s.mu.Lock()
				apID := fmt.Sprintf("%s%s", prefixApproval, approvalID)
				s.pendingApproval = &approvalState{
					approvalID: approvalID,
					toolName:   toolName,
					port:       s.servePort,
				}
				s.mu.Unlock()

				text := fmt.Sprintf("%s 需要批准：%s", emoji, _escapeMdv2(label))
				onceData := fmt.Sprintf("%s:%s", apID, actionOnce)
				sessionData := fmt.Sprintf("%s:%s", apID, actionSession)
				denyData := fmt.Sprintf("%s:%s", apID, actionDeny)
				msg := newMessage(chatID, text)
				msg.ParseMode = "MarkdownV2"
				msg.ReplyMarkup = tgbotapi.NewInlineKeyboardMarkup(
					[]tgbotapi.InlineKeyboardButton{
						{Text: "✅ 批准一次", CallbackData: &onceData},
						{Text: "🔒 始终批准", CallbackData: &sessionData},
					},
					[]tgbotapi.InlineKeyboardButton{
						{Text: "❌ 拒绝", CallbackData: &denyData},
					},
				)
				a.sendSafe(msg)
			},
			func(u wireUsage) {
				// onUsage: accumulate session totals + store latest for /status.
				s.mu.Lock()
				s.lastUsage = u
				s.cumPrompt += u.PromptTokens
				s.cumCompletion += u.CompletionTokens
				s.cumTotal += u.TotalTokens
				s.cumCost += u.Cost
				if u.Currency != "" {
					s.cumCurrency = u.Currency
				}
				s.mu.Unlock()
			},
		)
	}()

	pusherDone := make(chan struct{})
	go func() {
		defer close(pusherDone)
		debounce := time.NewTimer(time.Hour)
		debounce.Stop()
		stopDebounce := func() {
			if !debounce.Stop() {
				select {
				case <-debounce.C:
				default:
				}
			}
		}
		flushAndEnd := func() {
			stopDebounce()
			pushDraft()
			endStream()
		}
		// newSegmentHandler finalizes the current text as a complete message,
		// resets the buffer, and continues streaming in a new bubble.
		// Used at tool boundaries (tool mode only).
		newSegmentHandler := func() {
			stopDebounce()
			// Tool boundary: keep streaming even if a stray onComplete slipped through.
			draftMu.Lock()
			streamDone = false
			segDraftID := draftID
			segDraftShown := draftShown
			segLiveDraftEver := liveDraftEver
			segUseDraft := useDraft
			segStreamMsgID := streamMsgID
			draftMu.Unlock()
			pushDraft()
			draftMu.Lock()
			bufMu.Lock()
			body := strings.TrimSpace(buf.String())
			buf.Reset()
			truncated = false
			bufMu.Unlock()
			segHadLiveDraft := draftNeedsCleanup(segDraftShown || draftShown, segLiveDraftEver || liveDraftEver, lastDraftBody)
			if body != "" {
				if segUseDraft {
					a.finalizeDraft(chatID, segDraftID, body, segHadLiveDraft)
				} else if segStreamMsgID > 0 {
					a.sendTextParts(chatID, body, &segStreamMsgID)
				} else {
					a.sendTextParts(chatID, body, nil)
				}
				replyDelivered = true
			}
			if segHadLiveDraft {
				a.clearDraftPreview(chatID, segDraftID)
			}
			// Post-tool segments use edit-in-place, not native drafts — an open
			// sendMessageDraft blocks the user from replying until Telegram times it out.
			draftID = a.nextDraftID()
			useDraft = false
			draftShown = false
			liveDraftEver = false
			lastDraftBody = ""
			streamMsgID = 0
			streamEditFallback = false
			streamVisiblePrefix = ""
			msgCreatedAt = time.Now()
			draftMu.Unlock()
		}
		drainFlush := func() bool {
			select {
			case <-flushNow:
				log.Printf("chat=%d: pusher: flushNow", chatID)
				flushAndEnd()
				return true
			default:
				return false
			}
		}
		for {
			// turn_done signals flushNow; drain it before finished so we never
			// mark streamDone on an empty pre-empt while content is still pending.
			if drainFlush() {
				continue
			}
			select {
			case <-pushWake:
				log.Printf("chat=%d: pusher: pushWake", chatID)
				stopDebounce()
				debounce.Reset(streamDebounce)
			case <-debounce.C:
				log.Printf("chat=%d: pusher: debounce fire", chatID)
				pushDraft()
			case <-newSegment:
				log.Printf("chat=%d: pusher: newSegment", chatID)
				newSegmentHandler()
			case <-flushNow:
				log.Printf("chat=%d: pusher: flushNow", chatID)
				flushAndEnd()
			case <-finished:
				log.Printf("chat=%d: pusher: finished", chatID)
				if drainFlush() {
					continue
				}
				flushAndEnd()
				draftMu.Lock()
				done := streamDone
				draftMu.Unlock()
				if done {
					return
				}
				// runServeTurn returned before turn_done flush; wait briefly for it.
				select {
				case <-flushNow:
					log.Printf("chat=%d: pusher: late flushNow after finished", chatID)
					flushAndEnd()
				case <-time.After(3 * time.Second):
					log.Printf("chat=%d: pusher: finished without finalize, giving up", chatID)
				}
				return
			}
		}
	}()

	select {
	case <-pusherDone:
	case <-ctx.Done():
		select {
		case <-pusherDone:
		case <-time.After(8 * time.Second):
			if procErr == nil {
				procErr = ctx.Err()
			}
		}
	}

	if procErr != nil {
		draftMu.Lock()
		if draftNeedsCleanup(draftShown, liveDraftEver, lastDraftBody) {
			a.clearDraftPreview(chatID, draftID)
			draftShown = false
			liveDraftEver = false
		}
		streamDone = true
		draftMu.Unlock()
		if replyDelivered && errors.Is(procErr, context.Canceled) {
			log.Printf("chat=%d prompt=%q: canceled after reply delivered (draft cleared)", chatID, logPreview(prompt, 80))
			return
		}
		msg := fmt.Sprintf("请求失败：%s", userFacingError(procErr))
		if errors.Is(procErr, context.DeadlineExceeded) {
			msg = fmt.Sprintf("超时（%d 分钟）", a.cfg.MaxDuration)
		} else if errors.Is(procErr, context.Canceled) {
			msg = "已中止"
		}
		a.reply(chatID, msg)
		log.Printf("chat=%d prompt=%q err=%v", chatID, logPreview(prompt, 80), procErr)
		return
	}

	bufMu.Lock()
	empty := strings.TrimSpace(buf.String()) == ""
	bufMu.Unlock()

	// Silence detection: if the only reply was silence narration, suppress it.
	if !empty && isSilenceOnly(buf.String()) {
		log.Printf("chat=%d: suppressed silence-only reply", chatID)
		empty = true
	}

	log.Printf("chat=%d: finalCheck empty=%v replyDelivered=%v procErr=%v", chatID, empty, replyDelivered, procErr)
	if empty && !replyDelivered {
		a.reply(chatID, "（模型处理完成，但没有生成可见回复。请再发一次或换种问法。）")
	}
	bufMu.Lock()
	finalBody := strings.TrimSpace(buf.String())
	bufMu.Unlock()
	log.Printf("chat=%d prompt=%q stream=draft draftID=%d finalLen=%d runes=%d body=%q",
		chatID, logPreview(prompt, 80), draftID, len(finalBody), utf8.RuneCountInString(finalBody), logPreview(finalBody, 200))
}

// streamFinalizeBody picks the text to finalize at turn end. The accumulator
// buffer can lag behind or be reset while sendMessageDraft already shows text
// in lastDraftBody — falling back prevents a stuck draft with no sendMessage.
func streamFinalizeBody(buf, lastDraftBody string) string {
	body := strings.TrimSpace(buf)
	if body == "" {
		body = strings.TrimSpace(lastDraftBody)
	}
	return body
}

// nextDraftID returns a unique Telegram draft_id (int32-safe, no second-level collisions).
func (a *App) nextDraftID() int64 {
	seq := atomic.AddUint64(&a.draftSeq, 1)
	// Low 9 digits from unix seconds + 4-digit sequence within the same second.
	return int64(time.Now().Unix()%1_000_000_000)*10000 + int64(seq%10000)
}

func draftHadPreview(lastDraftBody string) bool {
	return strings.TrimSpace(lastDraftBody) != ""
}

func draftNeedsCleanup(draftShown, liveDraftEver bool, lastDraftBody string) bool {
	_ = lastDraftBody // edit-in-place preview; not a native sendMessageDraft
	return draftShown || liveDraftEver
}

// clearDraftPreview retires a live sendMessageDraft bubble. Only safe when a
// non-empty preview was previously sent for this draft_id — empty dismiss on a
// never-shown draft creates a brief ghost bubble on Telegram.
func (a *App) clearDraftPreview(chatID int64, draftID int64) {
	if draftID == 0 {
		return
	}
	a.dismissDraft(chatID, draftID)
	a.clearSessionDraft(chatID, draftID)
	log.Printf("chat=%d draftID=%d: cleared draft preview", chatID, draftID)
}

// finalizeDraft ends a native-draft segment with sendMessage (Hermes pattern).
// sendMessage first so a failed HTML format does not dismiss the live preview;
// dismiss the draft only after the real message lands.
func (a *App) finalizeDraft(chatID int64, draftID int64, text string, hadLiveDraft bool) int {
	if strings.TrimSpace(text) == "" {
		if hadLiveDraft {
			a.clearDraftPreview(chatID, draftID)
		}
		return 0
	}
	if hadLiveDraft {
		a.clearDraftPreview(chatID, draftID)
	}
	return a.sendTextParts(chatID, text, nil)
}

func (a *App) trackSessionDraft(chatID int64, draftID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	s.liveDraftID = draftID
	s.mu.Unlock()
}

func (a *App) clearSessionDraft(chatID int64, draftID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	if s.liveDraftID == draftID {
		s.liveDraftID = 0
	}
	s.mu.Unlock()
}

func (a *App) dismissSessionDraft(chatID int64) {
	s := a.getOrCreateSession(chatID)
	s.mu.Lock()
	draftID := s.liveDraftID
	s.liveDraftID = 0
	s.mu.Unlock()
	if draftID == 0 {
		return
	}
	a.dismissDraft(chatID, draftID)
	log.Printf("chat=%d: dismissed session draftID=%d (pre-empt/stale cleanup)", chatID, draftID)
}

// sendDraft pushes streaming preview text via sendMessageDraft (Bot API 9.5+).
// Text is automatically converted from markdown to Telegram HTML format.
func (a *App) sendDraft(chatID int64, draftID int64, text string) bool {
	text = formatForTelegram(text)
	text = telegramPreviewTail(text, telegramMaxMessageRunes)
	if text == "" {
		return false
	}
	_, err := a.bot.MakeRequest("sendMessageDraft", tgbotapi.Params{
		"chat_id":    strconv.FormatInt(chatID, 10),
		"draft_id":   strconv.FormatInt(draftID, 10),
		"text":       text,
		"parse_mode": "MarkdownV2",
	})
	if err != nil {
		log.Printf("sendMessageDraft failed (fallback to edit): %v", err)
		return false
	}
	return true
}

// dismissDraft clears a native draft preview by sending an empty sendMessageDraft.
func (a *App) dismissDraft(chatID int64, draftID int64) {
	_, _ = a.bot.MakeRequest("sendMessageDraft", tgbotapi.Params{
		"chat_id":  strconv.FormatInt(chatID, 10),
		"draft_id": strconv.FormatInt(draftID, 10),
		"text":     "",
	})
}
