// Media group aggregation for reasonix-telegram.
// Batches consecutive photos sharing a media_group_id into a single prompt,
// instead of processing each photo as an independent message.
package main

import (
	"fmt"
	"log"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const mediaBatchDelay = 800 * time.Millisecond

type photoEntry struct {
	FileID  string
	Caption string
}

type mediaGroupBatch struct {
	mediaGroupID string
	chatID       int64
	photos       []photoEntry
	timer        *time.Timer
	flushing     bool
}

// enqueueMediaGroup registers a photo into a media group batch.
// Returns true if the message was enqueued (caller should not process further),
// false if the photo has no media_group_id or should be handled normally.
func (a *App) enqueueMediaGroup(m *tgbotapi.Message) bool {
	if m.Photo == nil || len(m.Photo) == 0 || m.MediaGroupID == "" {
		return false
	}

	chatID := m.Chat.ID
	groupID := m.MediaGroupID
	largest := m.Photo[len(m.Photo)-1]
	caption := m.Caption

	a.mediaGroupsMu.Lock()
	defer a.mediaGroupsMu.Unlock()

	// Ensure per-chat map exists
	if a.mediaGroups[chatID] == nil {
		a.mediaGroups[chatID] = make(map[string]*mediaGroupBatch)
	}

	batch, exists := a.mediaGroups[chatID][groupID]
	if !exists {
		batch = &mediaGroupBatch{
			mediaGroupID: groupID,
			chatID:       chatID,
		}
		a.mediaGroups[chatID][groupID] = batch
	}

	// Stop existing timer
	if batch.timer != nil {
		batch.timer.Stop()
	}

	// Add photo
	batch.photos = append(batch.photos, photoEntry{
		FileID:  largest.FileID,
		Caption: caption,
	})

	// Start new timer — flush after 800ms of inactivity
	batch.timer = time.AfterFunc(mediaBatchDelay, func() {
		a.flushMediaGroup(chatID, groupID)
	})

	log.Printf("chat=%d: enqueued photo %d in group %s", chatID, len(batch.photos), groupID)
	return true
}

// flushMediaGroup downloads all photos in a batch and sends the aggregated prompt.
func (a *App) flushMediaGroup(chatID int64, groupID string) {
	a.mediaGroupsMu.Lock()
	batch, ok := a.mediaGroups[chatID][groupID]
	if !ok || batch.flushing {
		a.mediaGroupsMu.Unlock()
		return
	}
	batch.flushing = true
	delete(a.mediaGroups[chatID], groupID)
	if len(a.mediaGroups[chatID]) == 0 {
		delete(a.mediaGroups, chatID)
	}
	a.mediaGroupsMu.Unlock()

	// Download all photos and build prompt
	var paths []string
	var captions []string
	for _, p := range batch.photos {
		path, err := a.downloadTelegramFile(p.FileID, ".jpg", "images", chatID)
		if err != nil {
			log.Printf("chat=%d: batch download photo: %v", chatID, err)
			path = "(下载失败)"
		}
		paths = append(paths, path)
		if p.Caption != "" {
			captions = append(captions, p.Caption)
		}
	}

	// Build aggregated prompt
	prompt := fmt.Sprintf("[用户发送了 %d 张照片，保存在：\n", len(paths))
	for i, p := range paths {
		prompt += fmt.Sprintf("%d. %s\n", i+1, p)
	}
	prompt += "]"

	// Append captions if any
	for _, c := range captions {
		prompt += "\n" + c
	}

	log.Printf("chat=%d: flushing media group %s (%d photos)", chatID, groupID, len(paths))

	a.runTask(chatID, 0, prompt)
}
