// Sticker caching for reasonix-telegram.
// Downloads static stickers to local cache, injects file paths for agent vision,
// and uses emoji placeholders for animated/video stickers.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// StickerCacheEntry caches sticker metadata to avoid re-downloading.
type StickerCacheEntry struct {
	Description string    `json:"description"`
	Emoji       string    `json:"emoji"`
	SetName     string    `json:"set_name"`
	CachedAt    time.Time `json:"cached_at"`
}

// stickerCachePath returns the path to the global sticker cache JSON file.
func (a *App) stickerCachePath() string {
	return filepath.Join(a.cfg.StateDir, "sticker_cache.json")
}

// loadStickerCache reads the sticker cache from disk.
func (a *App) loadStickerCache() map[string]StickerCacheEntry {
	cache := make(map[string]StickerCacheEntry)
	b, err := os.ReadFile(a.stickerCachePath())
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("sticker cache load: %v", err)
		}
		return cache
	}
	if err := json.Unmarshal(b, &cache); err != nil {
		log.Printf("sticker cache parse: %v", err)
	}
	return cache
}

// saveStickerCache atomically writes the sticker cache to disk.
func (a *App) saveStickerCache(cache map[string]StickerCacheEntry) {
	path := a.stickerCachePath()
	b, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		log.Printf("sticker cache marshal: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("sticker cache write tmp: %v", err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("sticker cache rename: %v", err)
	}
}

// handleSticker processes an incoming sticker message.
// For static stickers: downloads the file and injects the path for agent vision.
// For animated/video stickers: injects an emoji placeholder.
func (a *App) handleSticker(m *tgbotapi.Message) string {
	if m.Sticker == nil {
		return ""
	}

	a.stickerMu.Lock()
	defer a.stickerMu.Unlock()

	fileUniqueID := m.Sticker.FileUniqueID
	emoji := m.Sticker.Emoji
	setName := m.Sticker.SetName

	// Check cache first
	cache := a.loadStickerCache()
	if entry, ok := cache[fileUniqueID]; ok {
		if entry.Description != "" {
			if _, err := os.Stat(entry.Description); err == nil {
				return fmt.Sprintf("[用户发送了贴纸 %s 来自 \"%s\"~ 已缓存在: %s]", emoji, entry.SetName, entry.Description)
			}
			delete(cache, fileUniqueID)
			a.saveStickerCache(cache)
		}
	}

	// Static sticker: download and cache (WEBP).
	// Animated stickers (TGS Lottie) can't be meaningfully displayed — use emoji placeholder.
	if !m.Sticker.IsAnimated {
		path, err := a.downloadTelegramFile(m.Sticker.FileID, ".webp", "stickers", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download sticker: %v", m.Chat.ID, err)
			return fmt.Sprintf("[用户发送了贴纸 %s]", emoji)
		}

		// Cache the entry
		cache[fileUniqueID] = StickerCacheEntry{
			Description: path,
			Emoji:       emoji,
			SetName:     setName,
			CachedAt:    time.Now(),
		}
		a.saveStickerCache(cache)

		return fmt.Sprintf("[用户发送了贴纸 %s 来自 \"%s\"~ 保存在 %s]", emoji, setName, path)
	}

	// Animated/video sticker: emoji-only placeholder
	return fmt.Sprintf("[用户发送了动画贴纸 %s 来自 \"%s\"~ 无法预览动画内容]", emoji, setName)
}
