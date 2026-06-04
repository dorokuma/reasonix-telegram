package main

import (
	"encoding/json"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	msgRestarting = "🔄 服务重启中，请稍候…"
	msgConnected  = "🟢 服务已连接，桥接已恢复。"
)

type restartNotifyFile struct {
	NotifyChats []int64 `json:"notify_chats"`
}

func restartNotifyPath(stateDir string) string {
	return filepath.Join(stateDir, "restart_notify.json")
}

func saveRestartNotify(stateDir string, chatID int64) error {
	path := restartNotifyPath(stateDir)
	var nf restartNotifyFile
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &nf)
	}
	for _, id := range nf.NotifyChats {
		if id == chatID {
			return writeRestartNotify(path, nf)
		}
	}
	nf.NotifyChats = append(nf.NotifyChats, chatID)
	return writeRestartNotify(path, nf)
}

func writeRestartNotify(path string, nf restartNotifyFile) error {
	b, err := json.MarshalIndent(nf, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadRestartNotify(stateDir string) ([]int64, error) {
	path := restartNotifyPath(stateDir)
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var nf restartNotifyFile
	if err := json.Unmarshal(b, &nf); err != nil {
		return nil, err
	}
	return nf.NotifyChats, nil
}

func clearRestartNotify(stateDir string) {
	_ = os.Remove(restartNotifyPath(stateDir))
}

func (a *App) notifyBridgeRestarted() {
	chats, err := loadRestartNotify(a.cfg.StateDir)
	if err != nil {
		log.Printf("restart notify load: %v", err)
		return
	}
	if len(chats) == 0 {
		return
	}
	for _, chatID := range chats {
		a.reply(chatID, msgConnected)
		log.Printf("chat=%d: sent post-restart connected notice", chatID)
	}
	clearRestartNotify(a.cfg.StateDir)
}

func (a *App) anyTaskRunning() bool {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	for _, s := range a.sess {
		s.mu.Lock()
		busy := s.task != nil
		s.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

func (a *App) cancelAllTasks() {
	a.sessMu.Lock()
	defer a.sessMu.Unlock()
	for _, s := range a.sess {
		s.mu.Lock()
		if t := s.task; t != nil {
			t.cancel()
		}
		s.mu.Unlock()
	}
}

func (a *App) stopAllServes() {
	seen := map[int64]struct{}{}
	if records, err := a.state.load(); err == nil {
		for _, r := range records {
			seen[r.ChatID] = struct{}{}
			a.stopServe(r.ChatID)
		}
	}
	a.sessMu.Lock()
	for chatID := range a.sess {
		if _, ok := seen[chatID]; !ok {
			a.stopServe(chatID)
		}
	}
	a.sessMu.Unlock()
}

func (a *App) waitTasksDone(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !a.anyTaskRunning() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("WARN: graceful restart proceeding with tasks still running")
}

// gracefulServiceRestart stops in-flight work and reasonix serve processes,
// notifies the user, then asks systemd to restart this unit.
func (a *App) gracefulServiceRestart(chatID int64) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(chatID, "⏳ 已在重启中，请稍候…")
		return
	}
	a.restarting = true
	a.restartMu.Unlock()

	a.reply(chatID, msgRestarting)
	if err := saveRestartNotify(a.cfg.StateDir, chatID); err != nil {
		log.Printf("save restart notify: %v", err)
	}

	a.cancelAllTasks()
	a.waitTasksDone(8 * time.Second)
	a.stopAllServes()
	// Let reasonix serve flush session JSONL after SIGTERM (stopAllServes waits up to 5s).
	time.Sleep(800 * time.Millisecond)

	log.Printf("chat=%d: initiating graceful systemd restart", chatID)
	go func() {
		time.Sleep(600 * time.Millisecond)
		cmd := exec.Command("systemctl", "restart", "reasonix-telegram.service")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("systemctl restart failed: %v (%s)", err, string(out))
			a.restartMu.Lock()
			a.restarting = false
			a.restartMu.Unlock()
			a.reply(chatID, "❌ 重启失败，请检查服务器日志。")
		}
	}()
}