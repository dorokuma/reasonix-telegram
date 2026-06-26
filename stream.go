package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
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

// runTask streams the model reply into one Telegram bubble (sendRichMessageDraft when
// supported, else editMessageText). No "running/done" status prefix — only text.
func (a *App) runTask(chatID int64, replyTo int, prompt string) {
	s := a.getOrCreateSession(chatID)

	// Construct thisTask before locking (cancel function must be ready).
	var ctx context.Context
	var cancel context.CancelFunc
	if a.cfg.MaxDuration > 0 {
		ctx, cancel = context.WithTimeout(context.Background(), time.Duration(a.cfg.MaxDuration)*time.Minute)
	} else {
		ctx, cancel = context.WithCancel(context.Background())
	}
	thisTask := &runningTask{cancel: cancel}

	// Atomically: cancel any previous task and set the new one under a single
	// lock, eliminating the TOCTOU window between cancellation and assignment.
	// Cancel the old task's cancel func outside the lock (may block).
	var oldCancel context.CancelFunc
	s.mu.Lock()
	s.lastActivity = time.Now()
	if t := s.task; t != nil {
		log.Printf("chat=%d: pre-empting running turn", chatID)
		oldCancel = t.cancel
	}
	s.task = thisTask
	s.mu.Unlock()

	if oldCancel != nil {
		oldCancel()
	}

	a.dismissSessionDraft(chatID)

	if err := a.ensureServe(chatID); err != nil {
		// Clean up thisTask since we won't proceed.
		s.mu.Lock()
		if s.task == thisTask {
			s.task = nil
		}
		s.mu.Unlock()
		cancel()
		a.reply(chatID, fmt.Sprintf("Reasonix 服务启动失败: %v", err))
		return
	}

	stopTyping := a.beginTyping(chatID)
	defer stopTyping()

	defer func() {
		s.mu.Lock()
		// Only clear if we're still the owner — a newer turn may have replaced us.
		if s.task == thisTask {
			s.task = nil
			s.wakePusher = nil
		}
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
		draftShown     bool // sendRichMessageDraft succeeded for current draftID
		liveDraftEver  bool // any sendRichMessageDraft succeeded this segment (survives state resets)
		streamDone     bool
		lastDraftBody  string
		msgCreatedAt   time.Time // when first draft/stream msg was sent
		editFailCount        int  // consecutive edit failures in this turn
		draftFailCount       int  // consecutive draft failures in this turn
		streamEditFallback   bool // edit flood-silenced: finalize via sendMessage tail
		streamVisiblePrefix  string // last raw preview successfully shown (edit/draft)
		leakProbe            strings.Builder // accumulates turn-start text for leak detection
		leakDecided          bool // language decision made for this segment
		leakDetected         bool // thinking-leak detected — drop all text this segment
		leakTail             string // incomplete UTF-8 tail saved across chunks
	)
	const (
		maxDraftFailures = 3
		maxEditFailures  = 3
		freshFinalAfter  = 30 * time.Second
		streamDebounce   = 50 * time.Millisecond
	)
	var procErr error
	var replyDelivered atomic.Bool
	lastSentBody := "" // tracks last finalized text to prevent duplicate sends
	releaseTask := func() {
		s.mu.Lock()
		if s.task == thisTask {
			s.task = nil
			s.wakePusher = nil
		}
		s.mu.Unlock()
	}
	// endStream mirrors TelePi finalizeResponse: set streamDone first, flush last
	// draft frame, then sendMessage so no late sendRichMessageDraft lands after the real message.
	// Fresh final: if the first preview was sent >30s ago, create a new message
	// instead of editing the stale preview (so TG timestamp reflects completion time).
	endStream := func() {
		draftMu.Lock()
		defer draftMu.Unlock()
		if streamDone {
			return
		}
		hadPreview := draftShown || liveDraftEver
		if hadPreview {
			a.clearDraftPreview(chatID, draftID)
			draftShown = false
			liveDraftEver = false
		}

		bufMu.Lock()
		raw := buf.String()
		tr := truncated
		bufMu.Unlock()
		body := streamFinalizeBody(raw, lastDraftBody)
		body = stripErrorLines(body)
		body = stripBackgroundJobs(body)
		if body != "" && strings.TrimSpace(raw) == "" && strings.TrimSpace(lastDraftBody) != "" {
			log.Printf("chat=%d: endStream using lastDraftBody fallback len=%d", chatID, len(body))
		}
		log.Printf("chat=%d: endStream useDraft=%v draftID=%d bodyLen=%d bodyPreview=%q", chatID, useDraft, draftID, len(body), logPreview(body, 100))
		// Dedup: skip if the pusher already sent this exact text.
		if body == lastSentBody {
			log.Printf("chat=%d: endStream dedup — body matches lastSentBody, skipping", chatID)
			streamDone = true
			releaseTask()
			return
		}
		// Silence marker: model returned [SILENT] or NO_REPLY — suppress sending.
		if isIntentionalSilence(body) {
			log.Printf("chat=%d: silence detected, suppressing send", chatID)
			streamDone = true
			releaseTask()
			return
		}
		if body == "" {
			if hadPreview {
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
			n = a.finalizeDraft(chatID, draftID, body, false)
			if n > 0 {
				draftShown = false
				liveDraftEver = false
				lastDraftBody = ""
			}
			log.Printf("chat=%d draftID=%d: finalize %d part(s) total=%d runes", chatID, draftID, n, utf8.RuneCountInString(body))
		} else {
			hadLiveDraft := draftShown || liveDraftEver
			if useFreshFinal && streamMsgID > 0 {
				// Stale preview — upgrade it to rich text in-place instead of
				// deleting + resending (avoids duplicate messages).
				log.Printf("chat=%d: fresh final (stale preview >%ds), upgrading in-place", chatID, int(freshFinalAfter.Seconds()))
				editID := streamMsgID
				n = a.sendTextParts(chatID, body, &editID)
			} else if streamMsgID > 0 {
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
					streamed := telegramPreviewTail(body, telegramMaxMessageRunes)
					if streamed == lastDraftBody {
						// Stream showed plain text via draft. Edit the existing message
						// to upgrade it to Rich Messages formatting.
						log.Printf("chat=%d: finalize upgrade stream to Rich Messages", chatID)
						editID := streamMsgID
						n = a.sendTextParts(chatID, body, &editID)
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
				log.Printf("chat=%d: finalize send failed (0 parts), trying plain text fallback", chatID)
				plain := stripMdv2(body)
				if plain != "" {
					fallback := newMessage(chatID, capTelegramMessage(plain))
					fallback.ParseMode = ""
					sent, err := a.sendWithRetry(fallback, chatID)
					if err == nil {
						n = 1
						replyDelivered.Store(true)
						lastSentBody = body
						a.recordSentText(sent.MessageID, plain)
						log.Printf("chat=%d: finalize plain text fallback succeeded msgID=%d", chatID, sent.MessageID)
					} else {
						log.Printf("chat=%d: finalize plain text fallback also failed: %v", chatID, err)
					}
				}
				if n == 0 {
					log.Printf("chat=%d: finalize send failed after fallback, closing stream", chatID)
					a.reply(chatID, "（回复生成完成但发送失败，请重试。）")
					streamDone = true
					releaseTask()
				}
			}
		}
		if n > 0 {
			replyDelivered.Store(true)
			lastSentBody = body
			streamDone = true
			releaseTask()
		}
	}

	retireLiveDraftLocked := func(reason string) {
		if !useDraft {
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
		// Native drafts and edit-in-place must not mix: an open sendRichMessageDraft
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
				s.mu.Lock()
				s.liveDraftID = draftID
				s.mu.Unlock()
				if msgCreatedAt.IsZero() {
					msgCreatedAt = time.Now()
				}
				return
			}
			draftFailCount++
			if draftFailCount >= maxDraftFailures {
				log.Printf("chat=%d: disabling draft stream after %d failures", chatID, draftFailCount)
				retireLiveDraftLocked("draft_send_failed")
				useDraft = false
				draftID = a.nextDraftID()
			}
		}
		if streamMsgID == 0 {
			msg := newMessage(chatID, preview)
			msg.ParseMode = "" // plain text preview; final send uses Rich Messages
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
		edit := tgbotapi.NewEditMessageText(chatID, streamMsgID, preview)
		edit.ParseMode = "" // plain text edit; final send uses Rich Messages
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
		// Native sendRichMessageDraft holds the Telegram composer until dismissed — do not
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

	// resetLeakState clears the thinking-leak probe so the next text segment
	// (after a tool boundary or approval) gets a fresh detection window.
	resetLeakState := func() {
		leakProbe.Reset()
		leakDecided = false
		leakDetected = false
		leakTail = ""
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
				if leakDetected {
					// Thinking-leak: drop all text for this segment.
					bufMu.Unlock()
					return
				}
				if !leakDecided {
					// Prepend any incomplete UTF-8 tail saved from the previous chunk.
					if leakTail != "" {
						chunk = leakTail + chunk
						leakTail = ""
					}
					probe := leakProbe.String() + chunk
					decision := detectThinkingLeak(probe, false)
					switch decision {
					case leakDrop:
						leakDetected = true
						leakDecided = true
						log.Printf("chat=%d: thinking-leak detected, dropping segment prefix=%q", chatID, logPreview(probe, 120))
						bufMu.Unlock()
						return
					case leakKeep:
						leakDecided = true
						appendChunk(&buf, probe, a.cfg.MaxOutputBytes, &truncated)
						bufMu.Unlock()
						wakePush()
						return
					case leakUndecided:
						// Before writing to leakProbe, ensure we don't stash an
						// incomplete UTF-8 sequence at the end of the chunk.
						if !utf8.ValidString(chunk) {
							// Find the longest valid UTF-8 prefix by trimming
							// incomplete bytes from the tail (max 4 bytes for UTF-8).
							valid := chunk
							for i := len(chunk); i > 0 && i > len(chunk)-4; i-- {
								if utf8.ValidString(chunk[:i]) {
									valid = chunk[:i]
									break
								}
							}
							leakTail = chunk[len(valid):]
							chunk = valid
						}
						leakProbe.WriteString(chunk)
						bufMu.Unlock()
						return
					}
				}
				appendChunk(&buf, chunk, a.cfg.MaxOutputBytes, &truncated)
				bufMu.Unlock()
				wakePush()
			},
			signalFlush,
			func() {
				// onToolDispatch: finalize current text segment and start fresh
				bufMu.Lock()
				if !leakDecided {
					probe := leakProbe.String()
					if leakTail != "" {
						probe += leakTail
						leakTail = ""
					}
					if probe != "" {
						decision := detectThinkingLeak(probe, true)
						if decision == leakKeep {
							appendChunk(&buf, probe, a.cfg.MaxOutputBytes, &truncated)
						}
						leakDecided = true
					}
				}
				bufMu.Unlock()
				select {
				case newSegment <- struct{}{}:
				default:
				}
			},
			func(text string) int {
				// onCommentary: send a standalone message (tool progress, result).
				// Not part of the stream buffer — send immediately as new message.
				// Don't touch draftMu to avoid contention with pusher goroutine.
				// MDV2 path (Hermes pattern): formatMessage → MDV2 → plain fallback.
				text = capTelegramMessage(text)
				formatted := formatMessage(text)
				msg := newMessage(chatID, formatted)
				msg.ParseMode = tgbotapi.ModeMarkdownV2
				// In "important" notification mode, suppress notification for tool progress.
				if a.cfg.NotificationMode == "important" {
					msg.DisableNotification = true
				}
				sent, err := a.sendWithRetry(msg, chatID)
				if err != nil {
					// MDV2 parse error — strip formatting and retry as plain text.
					plain := stripMdv2(text)
					msg.ParseMode = ""
					msg.Text = plain
					sent, err = a.sendWithRetry(msg, chatID)
					if err != nil {
						log.Printf("chat=%d: commentary send failed: %v", chatID, err)
						return 0
					}
				}
				replyDelivered.Store(true)
				a.recordSentText(sent.MessageID, text)
				return sent.MessageID
			},
			func(askID string, questions []askQuestionData) {
				// onAskRequest: model wants user input (ask tool).
				if len(questions) == 0 {
					return
				}

				// Reset stream state so post-answer output can flow in a fresh draft.
				draftMu.Lock()
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				resetLeakState()
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
				var cidBytes [8]byte
				var cid string
				if _, err := rand.Read(cidBytes[:]); err == nil {
					cid = base64.RawURLEncoding.EncodeToString(cidBytes[:])
				} else {
					cid = strconv.FormatUint(atomic.AddUint64(&a.clarifySeq, 1), 36) // fallback
				}
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
				qText := escapeMdv2(strings.TrimSpace(q.Text))
				if qText == "" {
					qText = escapeMdv2(strings.TrimSpace(q.ID))
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
				msg.ParseMode = tgbotapi.ModeMarkdownV2
				if len(q.Options) > 0 {
					var rows [][]tgbotapi.InlineKeyboardButton
					for i, choice := range q.Options {
						payload := fmt.Sprintf("%s%s:%d", prefixClarify, cid, i)
						data := signCallback(s.hmacKey, payload)
						btnText := truncateForButton(fmt.Sprintf("%d. %s", i+1, choice))
						rows = append(rows, []tgbotapi.InlineKeyboardButton{
							{Text: btnText, CallbackData: &data},
						})
					}
					otherPayload := fmt.Sprintf("%s%s:%s", prefixClarify, cid, actionOther)
					otherData := signCallback(s.hmacKey, otherPayload)
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
				replyDelivered.Store(true)
			},
			func(approvalID, toolName string) {
				// onApprovalRequest: model needs user approval for a tool.
				// Finalize current stream content first.
				signalFlush()
				// Reset stream state so post-approval output can flow in a fresh draft.
				draftMu.Lock()
				draftShown = false
				liveDraftEver = false
				streamDone = false
				bufMu.Lock()
				buf.Reset()
				truncated = false
				resetLeakState()
				bufMu.Unlock()
				draftID = a.nextDraftID()
				useDraft = true
				lastDraftBody = ""
				streamMsgID = 0
				msgCreatedAt = time.Now()
				draftMu.Unlock()
				replyDelivered.Store(true)

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

				text := fmt.Sprintf("%s 需要批准：%s", emoji, escapeMdv2(label))
				oncePayload := fmt.Sprintf("%s:%s", apID, actionOnce)
				onceData := signCallback(s.hmacKey, oncePayload)
				sessionPayload := fmt.Sprintf("%s:%s", apID, actionSession)
				sessionData := signCallback(s.hmacKey, sessionPayload)
				denyPayload := fmt.Sprintf("%s:%s", apID, actionDeny)
				denyData := signCallback(s.hmacKey, denyPayload)
				msg := newMessage(chatID, text)
				msg.ParseMode = tgbotapi.ModeMarkdownV2
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
				// onUsage: use session-cumulative values (include sub-agent/planner stats).
				s.mu.Lock()
				s.lastUsage = u
				// Use session-cumulative fields when available (they include sub-agent
				// and planner token consumption); fall back to single-turn accumulation
				// for older serve versions that don't send session-cumulative tokens.
				if u.SessionPromptTokens > 0 || u.SessionTotalTokens > 0 {
					if u.SessionPromptTokens > s.cumPrompt {
						s.cumPrompt = u.SessionPromptTokens
					}
					if u.SessionTotalTokens > s.cumTotal {
						s.cumTotal = u.SessionTotalTokens
					}
					s.cumCompletion = s.cumTotal - s.cumPrompt
				} else {
					s.cumPrompt += u.PromptTokens
					s.cumCompletion += u.CompletionTokens
					s.cumTotal += u.TotalTokens
				}
				if u.SessionCost > 0 {
					s.cumCost = u.SessionCost
				} else {
					s.cumCost += u.Cost
				}
				if u.SessionCurrency != "" {
					s.cumCurrency = u.SessionCurrency
				} else if u.Currency != "" {
					s.cumCurrency = u.Currency
				}
				// Copy needed fields to local variables before releasing the lock.
				rec := chatRecord{
					ChatID:      chatID,
					Workdir:     s.workdir,
					SessionPath: a.state.sessionPathForChat(chatID),
					Port:        s.servePort,
					Model:       s.model,
					CumPrompt:   s.cumPrompt,
					CumComplete: s.cumCompletion,
					CumTotal:    s.cumTotal,
					CumCost:     s.cumCost,
					CumCurrency: s.cumCurrency,
					HMACKey:     base64.StdEncoding.EncodeToString(s.hmacKey),
				}
				s.mu.Unlock()
				// Persist cumulative values outside the lock so they survive restart.
				_ = a.state.upsert(rec)
			},
		)
		bufMu.Lock()
		if !leakDecided {
			probe := leakProbe.String()
			if leakTail != "" {
				probe += leakTail
				leakTail = ""
			}
			if probe != "" {
				decision := detectThinkingLeak(probe, true)
				if decision == leakKeep {
					appendChunk(&buf, probe, a.cfg.MaxOutputBytes, &truncated)
				}
				leakDecided = true
			}
		}
		bufMu.Unlock()
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
			segUseDraft := useDraft
			segStreamMsgID := streamMsgID
			draftMu.Unlock()
			pushDraft()
			draftMu.Lock()
			bufMu.Lock()
			body := strings.TrimSpace(buf.String())
			buf.Reset()
			truncated = false
			resetLeakState()
			bufMu.Unlock()
			segHadLiveDraft := draftShown || liveDraftEver
			if body != "" && body != lastSentBody {
				if segUseDraft {
					a.finalizeDraft(chatID, segDraftID, body, segHadLiveDraft)
				} else if segStreamMsgID > 0 {
					a.sendTextParts(chatID, body, &segStreamMsgID)
				} else {
					a.sendTextParts(chatID, body, nil)
				}
				lastSentBody = body
				replyDelivered.Store(true)
			}
			if segHadLiveDraft {
				a.clearDraftPreview(chatID, segDraftID)
			}
			// Post-tool segments use edit-in-place, not native drafts — an open
			// sendRichMessageDraft blocks the user from replying until Telegram times it out.
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
					debounce.Stop()
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
				debounce.Stop()
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
		streamDone = true
		draftMu.Unlock()
		a.dismissSessionDraft(chatID) // clear any live draft on error
		if replyDelivered.Load() && errors.Is(procErr, context.Canceled) {
			log.Printf("chat=%d prompt=%q: canceled after reply delivered (draft cleared)", chatID, "[content]")
			return
		}
		msg := fmt.Sprintf("请求失败：%s", userFacingError(procErr))
		if errors.Is(procErr, context.DeadlineExceeded) {
			msg = fmt.Sprintf("超时（%d 分钟）", a.cfg.MaxDuration)
		} else if errors.Is(procErr, context.Canceled) {
			msg = "已中止"
		}
		a.reply(chatID, msg)
		log.Printf("chat=%d prompt=%q err=%v", chatID, "[content]", procErr)
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

	log.Printf("chat=%d: finalCheck empty=%v replyDelivered=%v procErr=%v", chatID, empty, replyDelivered.Load(), procErr)
	if empty && !replyDelivered.Load() {
		a.reply(chatID, "（模型处理完成，但没有生成可见回复。请再发一次或换种问法。）")
	}
	bufMu.Lock()
	finalBody := strings.TrimSpace(buf.String())
	bufMu.Unlock()
	log.Printf("chat=%d prompt=%q stream=draft draftID=%d finalLen=%d runes=%d body=%q",
		chatID, "[content]", draftID, len(finalBody), utf8.RuneCountInString(finalBody), logPreview(finalBody, 200))
}

// streamFinalizeBody picks the text to finalize at turn end. The accumulator
// buffer can lag behind or be reset while sendRichMessageDraft already shows text
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



// clearDraftPreview clears session draft state. No API call needed —
// sendRichMessage replaces the draft automatically.
func (a *App) clearDraftPreview(chatID int64, draftID int64) {
	if draftID == 0 {
		return
	}
	a.dismissDraft(chatID, draftID)
	a.clearSessionDraft(chatID, draftID)
}

// finalizeDraft ends a stream segment by sending the final text.
// Rich Messages finalize replaces the draft automatically.
func (a *App) finalizeDraft(chatID int64, draftID int64, text string, hadLiveDraft bool) int {
	_ = draftID
	_ = hadLiveDraft
	if strings.TrimSpace(text) == "" {
		return 0
	}
	return a.sendTextParts(chatID, text, nil)
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
	if draftID != 0 {
		a.dismissDraft(chatID, draftID)
	}
}

// sendDraft pushes streaming preview via sendRichMessageDraft (Bot API 10.1+).
// The same draft_id auto-animates updates. Final sendRichMessage replaces the draft.
func (a *App) sendDraft(chatID int64, draftID int64, text string) bool {
	text = telegramPreviewTail(text, telegramMaxMessageRunes)
	if text == "" {
		return false
	}
	richMsg, err := marshalAPI(map[string]any{
		"markdown": text,
	})
	if err != nil {
		log.Printf("chat=%d: sendDraft marshal rich_message: %v", chatID, err)
		return false
	}
	_, err = a.bot.MakeRequest("sendRichMessageDraft", tgbotapi.Params{
		"chat_id":     strconv.FormatInt(chatID, 10),
		"draft_id":    strconv.FormatInt(draftID, 10),
		"rich_message": richMsg,
	})
	if err != nil {
		log.Printf("sendRichMessageDraft failed (fallback to edit): %v", err)
		return false
	}
	return true
}

func (a *App) dismissDraft(chatID int64, draftID int64) {
	richMsg, err := marshalAPI(map[string]any{
		"markdown": "",
	})
	if err != nil {
		log.Printf("chat=%d: dismissDraft marshal rich_message: %v", chatID, err)
		return
	}
	_, _ = a.bot.MakeRequest("sendRichMessageDraft", tgbotapi.Params{
		"chat_id":     strconv.FormatInt(chatID, 10),
		"draft_id":    strconv.FormatInt(draftID, 10),
		"rich_message": richMsg,
	})
}
