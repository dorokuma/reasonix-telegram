// Inbound media handling for reasonix-telegram.
// Downloads photos, documents, videos, GIFs, voice and audio from Telegram,
// caches them locally, and injects file paths into the agent prompt.
package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const maxMediaCachePerChat = 100

// cacheDir returns the cache directory for a given category and chat, creating it if needed.
func (a *App) cacheDir(category string, chatID int64) string {
	dir := filepath.Join(a.cfg.StateDir, "cache", category, fmt.Sprintf("%d", chatID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("chat=%d: cache dir %s: %v", chatID, dir, err)
	}
	return dir
}

// downloadTelegramFile downloads a file from Telegram by fileID into the local cache.
// Returns the local file path. Reuses cached files on subsequent calls.
func (a *App) downloadTelegramFile(fileID string, ext string, category string, chatID int64) (string, error) {
	tf, err := a.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("getFile: %w", err)
	}

	dir := a.cacheDir(category, chatID)
	localPath := filepath.Join(dir, fileID+ext)

	// Return cached file if it exists
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	// Download from Telegram
	url := tf.Link(a.bot.Token)
	resp, err := http.Get(url)
	if err != nil {
		return "", fmt.Errorf("download file %s: %w", fileID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download file %s: HTTP %d", fileID, resp.StatusCode)
	}

	f, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("create: %w", err)
	}
	defer f.Close()

	if _, err := io.Copy(f, resp.Body); err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("write: %w", err)
	}

	// LRU cleanup: remove oldest files if over limit
	a.cleanupCacheDir(category, chatID)

	return localPath, nil
}

// cleanupCacheDir removes oldest files when a cache directory exceeds the per-chat limit.
func (a *App) cleanupCacheDir(category string, chatID int64) {
	dir := a.cacheDir(category, chatID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) <= maxMediaCachePerChat {
		return
	}
	type entry struct {
		name string
		mod  time.Time
	}
	var list []entry
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		list = append(list, entry{e.Name(), info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].mod.Before(list[j].mod)
	})
	excess := len(list) - maxMediaCachePerChat
	for i := 0; i < excess; i++ {
		path := filepath.Join(dir, list[i].name)
		if err := os.Remove(path); err != nil {
			log.Printf("chat=%d: cache cleanup remove %s: %v", chatID, path, err)
		}
	}
}

// handleIncomingMedia checks a message for media content and returns a prompt fragment
// describing what was received, or empty string if there is no media.
func (a *App) handleIncomingMedia(m *tgbotapi.Message) string {
	switch {
	case m.Photo != nil && len(m.Photo) > 0:
		largest := m.Photo[len(m.Photo)-1]
		path, err := a.downloadTelegramFile(largest.FileID, ".jpg", "images", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download photo: %v", m.Chat.ID, err)
			return "[用户发送了图片，下载失败]"
		}
		return fmt.Sprintf("[用户发送了图片，保存在 %s]", path)

	case m.Document != nil:
		ext := ".dat"
		if m.Document.FileName != "" {
			if idx := strings.LastIndex(m.Document.FileName, "."); idx >= 0 {
				ext = m.Document.FileName[idx:]
			}
		}
		path, err := a.downloadTelegramFile(m.Document.FileID, ext, "docs", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download document: %v", m.Chat.ID, err)
			return fmt.Sprintf("[用户发送了文件 %s，下载失败]", m.Document.FileName)
		}
		return fmt.Sprintf("[用户发送了文件 %s，保存在 %s]", m.Document.FileName, path)

	case m.Video != nil:
		path, err := a.downloadTelegramFile(m.Video.FileID, ".mp4", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download video: %v", m.Chat.ID, err)
			return "[用户发送了视频，下载失败]"
		}
		return fmt.Sprintf("[用户发送了视频，保存在 %s]", path)

	case m.Animation != nil:
		path, err := a.downloadTelegramFile(m.Animation.FileID, ".gif", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download animation: %v", m.Chat.ID, err)
			return "[用户发送了GIF，下载失败]"
		}
		return fmt.Sprintf("[用户发送了GIF，保存在 %s]", path)

	case m.Voice != nil:
		path, err := a.downloadTelegramFile(m.Voice.FileID, ".ogg", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download voice: %v", m.Chat.ID, err)
			return "[用户发送了语音消息，下载失败]"
		}
		return fmt.Sprintf("[用户发送了语音消息，保存在 %s]", path)

	case m.Audio != nil:
		ext := ".mp3"
		if m.Audio.FileName != "" {
			if idx := strings.LastIndex(m.Audio.FileName, "."); idx >= 0 {
				ext = m.Audio.FileName[idx:]
			}
		}
		title := m.Audio.Title
		if title == "" {
			title = m.Audio.FileName
		}
		if title == "" {
			title = "音频"
		}
		path, err := a.downloadTelegramFile(m.Audio.FileID, ext, "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download audio: %v", m.Chat.ID, err)
			return "[用户发送了音频，下载失败]"
		}
		return fmt.Sprintf("[用户发送了音频 %s，保存在 %s]", title, path)
	}
	return ""
}
