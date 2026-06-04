package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const defaultStateDir = "/var/lib/reasonix-telegram"

// chatRecord is persisted across reasonix-telegram restarts so we can resume the
// same Reasonix conversation (reasonix serve --resume <path>).
type chatRecord struct {
	ChatID      int64  `json:"chat_id"`
	Workdir     string `json:"workdir"`
	SessionPath string `json:"session_path"`
	Port        int    `json:"port"`
}

type stateFile struct {
	Chats []chatRecord `json:"chats"`
}

type stateStore struct {
	mu   sync.Mutex
	dir  string
	path string
}

func newStateStore(dir string) (*stateStore, error) {
	if dir == "" {
		dir = defaultStateDir
	}
	for _, sub := range []string{"sessions", "cache", chatWorkdirSubdir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	return &stateStore{
		dir:  dir,
		path: filepath.Join(dir, "state.json"),
	}, nil
}

func (st *stateStore) sessionsDir() string {
	return filepath.Join(st.dir, "sessions")
}

func (st *stateStore) sessionPathForChat(chatID int64) string {
	return filepath.Join(st.sessionsDir(), fmt.Sprintf("%d.jsonl", chatID))
}

func (st *stateStore) load() ([]chatRecord, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	b, err := os.ReadFile(st.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, err
	}
	return sf.Chats, nil
}

func (st *stateStore) upsert(rec chatRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var sf stateFile
	if b, err := os.ReadFile(st.path); err == nil {
		_ = json.Unmarshal(b, &sf)
	}
	found := false
	for i, c := range sf.Chats {
		if c.ChatID == rec.ChatID {
			sf.Chats[i] = rec
			found = true
			break
		}
	}
	if !found {
		sf.Chats = append(sf.Chats, rec)
	}
	return st.writeLocked(&sf)
}

func (st *stateStore) remove(chatID int64) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var sf stateFile
	if b, err := os.ReadFile(st.path); err == nil {
		_ = json.Unmarshal(b, &sf)
	}
	out := sf.Chats[:0]
	for _, c := range sf.Chats {
		if c.ChatID != chatID {
			out = append(out, c)
		}
	}
	sf.Chats = out
	return st.writeLocked(&sf)
}

func (st *stateStore) writeLocked(sf *stateFile) error {
	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// sessionStats reads a Reasonix session JSONL for logging resume health.
func sessionStats(path string) (messages int, userTurns int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	if len(b) == 0 {
		return 0, 0, nil
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m struct {
			Role string `json:"role"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		messages++
		if m.Role == "user" {
			userTurns++
		}
	}
	return messages, userTurns, nil
}