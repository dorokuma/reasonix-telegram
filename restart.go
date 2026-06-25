package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

const (
	msgRestarting = "🔄 桥接重启中，请稍候..."
	msgConnected  = "✅ 桥接已重新就绪"
)

type restartNotifyFile struct {
	NotifyChats []int64 `json:"notify_chats"`
}

var restartNotifyMu sync.Mutex

func restartNotifyPath(stateDir string) string {
	return filepath.Join(stateDir, "restart_notify.json")
}

func saveRestartNotify(stateDir string, chatID int64) error {
	restartNotifyMu.Lock()
	defer restartNotifyMu.Unlock()
	path := restartNotifyPath(stateDir)
	var nf restartNotifyFile
	// Use O_NOFOLLOW to prevent symlink-following TOCTOU attacks between
	// the read and the subsequent write+rename.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err == nil {
		b, _ := io.ReadAll(f)
		f.Close()
		_ = json.Unmarshal(b, &nf)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("open restart notify: %w", err)
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
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		if err := os.Remove(tmp); err != nil {
			log.Printf("remove %s: %v", tmp, err)
		}
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		if err := os.Remove(tmp); err != nil {
			log.Printf("remove %s: %v", tmp, err)
		}
		return err
	}
	if err := f.Close(); err != nil {
		if err := os.Remove(tmp); err != nil {
			log.Printf("remove %s: %v", tmp, err)
		}
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if df, err := os.Open(dir); err == nil {
			_ = df.Sync()
			_ = df.Close()
		}
	}
	return nil
}

func loadRestartNotify(stateDir string) ([]int64, error) {
	restartNotifyMu.Lock()
	defer restartNotifyMu.Unlock()
	path := restartNotifyPath(stateDir)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var nf restartNotifyFile
	if err := json.Unmarshal(b, &nf); err != nil {
		return nil, err
	}
	return nf.NotifyChats, nil
}

func clearRestartNotify(stateDir string) {
	restartNotifyMu.Lock()
	defer restartNotifyMu.Unlock()
	if err := os.Remove(restartNotifyPath(stateDir)); err != nil {
		log.Printf("remove %s: %v", restartNotifyPath(stateDir), err)
	}
}

func (a *App) notifyBridgeRestarted() {
	// Collect chats to notify: those saved via graceful /restart + all restored sessions.
	notified := map[int64]bool{}

	chats, err := loadRestartNotify(a.cfg.StateDir)
	if err != nil {
		log.Printf("restart notify load: %v", err)
	}
	for _, c := range chats {
		notified[c] = true
	}

	// Also notify all active sessions (covers SIGTERM/systemctl restart).
	a.sessMu.Lock()
	for chatID := range a.sess {
		notified[chatID] = true
	}
	a.sessMu.Unlock()

	if len(notified) == 0 {
		return
	}
	for chatID := range notified {
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
	for chatID, s := range a.sess {
		if _, ok := seen[chatID]; !ok {
			a.stopSessionServe(s, chatID)
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

const restartUnit = "reasonix-telegram.service"

// triggerSystemRestart exits the process and lets systemd Restart=on-failure
// bring us back. We DO NOT call "systemctl restart" because systemd uses
// KillMode=control-group (default) which sends SIGTERM to all processes in
// the service cgroup, killing the systemctl child before its D-Bus call
// completes. Exiting directly with a non-zero code avoids this race entirely.
func triggerSystemRestart() {
	log.Printf("triggerSystemRestart: exiting for systemd auto-restart (Restart=on-failure)")
	os.Exit(1)
}

// gracefulServiceRestart stops in-flight work, persists who to notify on boot,
// then asks systemd to restart this unit (--no-block, detached session).
func (a *App) gracefulServiceRestart(chatID int64) {
	a.restartMu.Lock()
	if a.restarting {
		a.restartMu.Unlock()
		a.reply(chatID, "⏳ 已在重启中，请稍候...")
		return
	}
	a.restarting = true
	a.restartStarted = time.Now()
	a.restartMu.Unlock()

	// Persist first: the new process must read this after systemd replaces us.
	if err := saveRestartNotify(a.cfg.StateDir, chatID); err != nil {
		log.Printf("save restart notify: %v", err)
		a.restartMu.Lock()
		a.restarting = false
		a.restartMu.Unlock()
		a.reply(chatID, "❌ 无法保存重启通知，请检查数据目录权限。")
		return
	}
	a.reply(chatID, msgRestarting)

	go func() {
		a.restartingInProgress.Store(true)
		a.cancelAllTasks()
		a.waitTasksDone(30 * time.Second)
		a.stopAllServes()
		time.Sleep(500 * time.Millisecond)

		log.Printf("chat=%d: cleanup done, exiting for systemd auto-restart", chatID)
		triggerSystemRestart() // never returns
	}()
}

var restartWatch sync.Once

// startRestartWatchdog clears restarting if systemd restart never replaced the process.
func (a *App) startRestartWatchdog() {
	restartWatch.Do(func() {
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for range t.C {
				a.restartMu.Lock()
				stuck := a.restarting && time.Since(a.restartStarted) > 45*time.Second
				if stuck {
					a.restarting = false
					log.Printf("WARN: restart appears stuck >45s; accepting messages again")
				}
				a.restartMu.Unlock()
			}
		}()
	})
}