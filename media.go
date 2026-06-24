// Inbound media handling for reasonix-telegram.
// Downloads photos, documents, videos, GIFs, voice and audio from Telegram,
// caches them locally, and injects file paths into the agent prompt.
// For images/videos/PDFs, also provides base64-encoded data URLs so the
// model can "see" the content directly via multimodal vision support.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	maxMediaCachePerChat = 100
	maxDataURLSize       = 10 * 1024 * 1024 // 10 MB — skip base64 encoding beyond this
	maxTotalCacheSize    = 2 * 1024 * 1024 * 1024 // 2 GB — global LRU cache limit
)

// downloadSem limits concurrent Telegram file downloads to prevent OOM under load.
var downloadSem = make(chan struct{}, 3)

// promptSafeName truncates a filename to at most 64 characters for safe
// embedding in AI prompt text, preventing prompt injection via long filenames.
func promptSafeName(name string) string {
	if len(name) > 64 {
		return name[:64]
	}
	return name
}

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
// Concurrent downloads are throttled by downloadSem (cap 3) to limit peak memory.
func (a *App) downloadTelegramFile(fileID string, ext string, category string, chatID int64) (string, error) {
	tf, err := a.bot.GetFile(tgbotapi.FileConfig{FileID: fileID})
	if err != nil {
		return "", fmt.Errorf("getFile: %w", err)
	}

	const maxDownloadSize = 50 * 1024 * 1024 // 50MB limit
	if tf.FileSize > maxDownloadSize {
		return "", fmt.Errorf("file size %d exceeds limit of %d bytes", tf.FileSize, maxDownloadSize)
	}

	dir := a.cacheDir(category, chatID)
	localPath := filepath.Join(dir, fileID+ext)

	// Return cached file if it exists
	if _, err := os.Stat(localPath); err == nil {
		return localPath, nil
	}

	// Throttle concurrent downloads (semaphore acquire).
	downloadSem <- struct{}{}
	defer func() { <-downloadSem }()

	// Download from Telegram (use bot.Client which has tokenRedactingTransport)
	url := tf.Link(a.bot.Token)
	req, err := http.NewRequestWithContext(context.Background(), "GET", url, nil)
	if err != nil {
		return "", fmt.Errorf("download file %s: %w", fileID, err)
	}
	resp, err := a.bot.Client.Do(req)
	if err != nil {
		// Sanitize token from any error (e.g. timeout, DNS failure) that
		// might include the download URL with the bot token embedded.
		return "", fmt.Errorf("download file %s: %v", fileID, redactSecrets(err.Error(), a.cfg.secrets))
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

	limitReader := io.LimitReader(resp.Body, maxDownloadSize+1)
	written, err := io.Copy(f, limitReader)
	if err != nil {
		os.Remove(localPath)
		return "", fmt.Errorf("write: %w", err)
	}
	if written > maxDownloadSize {
		os.Remove(localPath)
		return "", fmt.Errorf("file size exceeds limit of %d bytes", maxDownloadSize)
	}

	// LRU cleanup: remove oldest files if over limit
	a.cleanupCacheDir(category, chatID)

	return localPath, nil
}

// cleanupCacheDir removes oldest files when a cache directory exceeds the per-chat limit
// or when the global cache directory exceeds maxTotalCacheSize.
func (a *App) cleanupCacheDir(category string, chatID int64) {
	dir := a.cacheDir(category, chatID)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	if len(entries) > maxMediaCachePerChat {
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

	// Global LRU check: if total cache exceeds maxTotalCacheSize, remove oldest files
	// across all categories and chats.
	cacheRoot := filepath.Join(a.cfg.StateDir, "cache")
	totalSize, files := a.walkCacheFiles(cacheRoot)
	if totalSize <= maxTotalCacheSize {
		return
	}
	// Sort by mod time ascending (oldest first).
	sort.Slice(files, func(i, j int) bool {
		return files[i].mod.Before(files[j].mod)
	})
	overage := totalSize - maxTotalCacheSize
	var removed int64
	for _, f := range files {
		if removed >= overage {
			break
		}
		info, err := os.Stat(f.path)
		if err != nil {
			continue
		}
		if err := os.Remove(f.path); err != nil {
			log.Printf("global cache cleanup remove %s: %v", f.path, err)
			continue
		}
		removed += info.Size()
	}
	if removed > 0 {
		log.Printf("global cache cleanup: removed %d bytes across %d files to stay under %d byte limit",
			removed, len(files), maxTotalCacheSize)
	}
}

type cacheFile struct {
	path string
	mod  time.Time
}

// walkCacheFiles walks cacheRoot recursively, collecting all files with their sizes and mod times.
func (a *App) walkCacheFiles(cacheRoot string) (int64, []cacheFile) {
	var total int64
	var files []cacheFile
	filepath.Walk(cacheRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil {
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		total += info.Size()
		files = append(files, cacheFile{path: path, mod: info.ModTime()})
		return nil
	})
	return total, files
}

// --- Multimodal encoding helpers ---

// dataURLFromFile reads a local file, base64-encodes it, and returns a
// data URL string suitable for the OpenAI image_url field, e.g.
// "data:image/jpeg;base64,/9j/4AAQ...".
// Supported extensions: .jpg, .jpeg, .png, .gif, .webp.
// Files larger than maxDataURLSize (10 MB) are skipped to avoid OOM.
func dataURLFromFile(path string) (string, error) {
	// Check file size before reading to avoid OOM on large files.
	fi, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if fi.Size() > maxDataURLSize {
		log.Printf("dataURLFromFile: skip %s, size=%d exceeds %d", path, fi.Size(), maxDataURLSize)
		return "", fmt.Errorf("file too large for data URL (%d > %d bytes)", fi.Size(), maxDataURLSize)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	log.Printf("dataURLFromFile: read %s, size=%d bytes", path, len(data))
	mime := mimeFromExt(path)
	if mime == "" {
		return "", fmt.Errorf("unsupported image extension: %s", filepath.Ext(path))
	}
	encoded := base64.StdEncoding.EncodeToString(data)
	log.Printf("dataURLFromFile: encoded to base64, size=%d bytes", len(encoded))
	return fmt.Sprintf("data:%s;base64,%s", mime, encoded), nil
}

// mimeFromExt returns the MIME type for common image file extensions.
func mimeFromExt(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	default:
		return ""
	}
}

// extractVideoFrames uses ffmpeg to extract one frame per second from the video
// at localPath, saves them as temporary JPEG files, and returns their data URLs.
// The temp files are cleaned up after encoding.
func extractVideoFrames(localPath string) ([]string, error) {
	// Create a temp directory for frames
	tmpDir, err := os.MkdirTemp("", "reasonix-video-frames-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// ffmpeg: output one JPEG per second at ~QVGA quality (scaled to 640 width)
	pattern := filepath.Join(tmpDir, "frame-%04d.jpg")
	cmd := exec.Command("ffmpeg",
		"-i", localPath,
		"-vf", "fps=1,scale=640:-1",
		"-q:v", "5",
		"-y", // overwrite
		pattern,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg: %v\n%s", err, stderr.String())
	}

	// Collect frames
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read frames: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("ffmpeg produced no frames from %s", localPath)
	}

	// Limit to at most 10 frames to avoid token overflow
	const maxFrames = 10
	var urls []string
	for _, e := range entries {
		if len(urls) >= maxFrames {
			break
		}
		framePath := filepath.Join(tmpDir, e.Name())
		url, err := dataURLFromFile(framePath)
		if err != nil {
			log.Printf("encode video frame %s: %v", framePath, err)
			continue
		}
		urls = append(urls, url)
	}
	return urls, nil
}

// convertPDFToImages uses pdftoppm to render each page of the PDF at 150 DPI
// as PNG images, then returns their data URLs. Temp files are cleaned up.
func convertPDFToImages(localPath string) ([]string, error) {
	tmpDir, err := os.MkdirTemp("", "reasonix-pdf-pages-*")
	if err != nil {
		return nil, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// pdftoppm: one PNG per page at 150 DPI
	prefix := filepath.Join(tmpDir, "page")
	cmd := exec.Command("pdftoppm",
		"-png",
		"-r", "150",
		localPath,
		prefix,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("pdftoppm: %v\n%s", err, stderr.String())
	}

	// Collect pages
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return nil, fmt.Errorf("read pages: %w", err)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("pdftoppm produced no pages from %s", localPath)
	}

	// Limit to at most 10 pages
	const maxPages = 10
	var urls []string
	for _, e := range entries {
		if len(urls) >= maxPages {
			break
		}
		pagePath := filepath.Join(tmpDir, e.Name())
		url, err := dataURLFromFile(pagePath)
		if err != nil {
			log.Printf("encode PDF page %s: %v", pagePath, err)
			continue
		}
		urls = append(urls, url)
	}
	return urls, nil
}

// mediaResult holds the result of processing an incoming media message.
type mediaResult struct {
	// Text is a human-readable description (e.g. "[用户发送了图片]")
	Text string
	// DataURLs contains base64-encoded data URLs for multimodal vision
	DataURLs []string
}

// handleIncomingMedia checks a message for media content and returns a
// mediaResult describing what was received, including base64-encoded data
// URLs for images/videos/PDFs so the model can see the content directly.
func (a *App) handleIncomingMedia(m *tgbotapi.Message) mediaResult {
	// --- Photo (image) ---
	if m.Photo != nil && len(m.Photo) > 0 {
		largest := m.Photo[len(m.Photo)-1]
		path, err := a.downloadTelegramFile(largest.FileID, ".jpg", "images", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download photo: %v", m.Chat.ID, err)
			return mediaResult{Text: "[用户发送了图片，下载失败]"}
		}
		desc := fmt.Sprintf("[用户发送了图片，保存在 %s]", path)
		urls, err := dataURLsFromImage(path)
		if err != nil {
			log.Printf("chat=%d: encode photo: %v", m.Chat.ID, err)
			return mediaResult{Text: desc}
		}
		log.Printf("chat=%d: photo encoded, urls len=%d", m.Chat.ID, len(urls))
		return mediaResult{Text: desc, DataURLs: urls}
	}

	// --- Document (including PDF) ---
	if m.Document != nil {
		ext := ".dat"
		if m.Document.FileName != "" {
			if e := filepath.Ext(m.Document.FileName); e != "" {
				switch strings.ToLower(e) {
				case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".pdf", ".ogg", ".mp3", ".dat":
					ext = e
				}
			}
		}
		path, err := a.downloadTelegramFile(m.Document.FileID, ext, "docs", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download document: %v", m.Chat.ID, err)
			return mediaResult{Text: fmt.Sprintf("[用户发送了文件 %s，下载失败]", promptSafeName(m.Document.FileName))}
		}

		lowerName := strings.ToLower(m.Document.FileName)
		if strings.HasSuffix(lowerName, ".pdf") {
			desc := fmt.Sprintf("[用户发送了PDF文件 %s，保存在 %s]", promptSafeName(m.Document.FileName), path)
			urls, err := convertPDFToImages(path)
			if err != nil {
				log.Printf("chat=%d: convert PDF: %v", m.Chat.ID, err)
				return mediaResult{Text: desc}
			}
			return mediaResult{Text: desc, DataURLs: urls}
		}

		// For image-type documents (PNG, JPG, etc.), send as image
		if mimeFromExt(path) != "" {
			desc := fmt.Sprintf("[用户发送了图片文件 %s，保存在 %s]", promptSafeName(m.Document.FileName), path)
			urls, err := dataURLsFromImage(path)
			if err != nil {
				log.Printf("chat=%d: encode document image: %v", m.Chat.ID, err)
				return mediaResult{Text: desc}
			}
			return mediaResult{Text: desc, DataURLs: urls}
		}

		return mediaResult{Text: fmt.Sprintf("[用户发送了文件 %s，保存在 %s]", promptSafeName(m.Document.FileName), path)}
	}

	// --- Video ---
	if m.Video != nil {
		path, err := a.downloadTelegramFile(m.Video.FileID, ".mp4", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download video: %v", m.Chat.ID, err)
			return mediaResult{Text: "[用户发送了视频，下载失败]"}
		}
		desc := fmt.Sprintf("[用户发送了视频，保存在 %s]", path)
		urls, err := extractVideoFrames(path)
		if err != nil {
			log.Printf("chat=%d: extract video frames: %v", m.Chat.ID, err)
			return mediaResult{Text: desc}
		}
		return mediaResult{Text: desc, DataURLs: urls}
	}

	// --- Animation (GIF) ---
	if m.Animation != nil {
		path, err := a.downloadTelegramFile(m.Animation.FileID, ".gif", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download animation: %v", m.Chat.ID, err)
			return mediaResult{Text: "[用户发送了GIF，下载失败]"}
		}
		desc := fmt.Sprintf("[用户发送了GIF，保存在 %s]", path)
		// Extract frames from GIF too
		urls, err := extractVideoFrames(path)
		if err != nil {
			log.Printf("chat=%d: extract GIF frames: %v", m.Chat.ID, err)
			return mediaResult{Text: desc}
		}
		return mediaResult{Text: desc, DataURLs: urls}
	}

	// --- Voice (audio only, no vision) ---
	if m.Voice != nil {
		path, err := a.downloadTelegramFile(m.Voice.FileID, ".ogg", "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download voice: %v", m.Chat.ID, err)
			return mediaResult{Text: "[用户发送了语音消息，下载失败]"}
		}
		return mediaResult{Text: fmt.Sprintf("[用户发送了语音消息，保存在 %s]", path)}
	}

	// --- Audio (no vision) ---
	if m.Audio != nil {
		ext := ".mp3"
		if m.Audio.FileName != "" {
			if e := filepath.Ext(m.Audio.FileName); e != "" {
				switch strings.ToLower(e) {
				case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".mp4", ".pdf", ".ogg", ".mp3", ".dat":
					ext = e
				}
			}
		}
		title := m.Audio.Title
		if title == "" {
			title = promptSafeName(m.Audio.FileName)
		}
		if title == "" {
			title = "音频"
		}
		path, err := a.downloadTelegramFile(m.Audio.FileID, ext, "media", m.Chat.ID)
		if err != nil {
			log.Printf("chat=%d: download audio: %v", m.Chat.ID, err)
			return mediaResult{Text: "[用户发送了音频，下载失败]"}
		}
		return mediaResult{Text: fmt.Sprintf("[用户发送了音频 %s，保存在 %s]", title, path)}
	}

	return mediaResult{}
}

// dataURLsFromImage reads an image file and returns a single-element slice
// containing its base64 data URL. This is a helper to unify the return type
// with the multi-frame video/PDF functions.
func dataURLsFromImage(path string) ([]string, error) {
	url, err := dataURLFromFile(path)
	if err != nil {
		return nil, err
	}
	return []string{url}, nil
}
