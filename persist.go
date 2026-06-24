package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const defaultStateDir = "/var/lib/reasonix-telegram"

// chatRecord is persisted across reasonix-telegram restarts so we can resume the
// same Reasonix conversation (reasonix serve --resume <path>).
type chatRecord struct {
	ChatID      int64   `json:"chat_id"`
	Workdir     string  `json:"workdir,omitempty"`
	SessionPath string  `json:"session_path"`
	Port        int     `json:"port"`
	Model       string  `json:"model,omitempty"` // per-chat model override, survives restart
	CumPrompt   int     `json:"cum_prompt,omitempty"`
	CumComplete int     `json:"cum_completion,omitempty"`
	CumTotal    int     `json:"cum_total,omitempty"`
	CumCost     float64 `json:"cum_cost,omitempty"`
	CumCurrency string  `json:"cum_currency,omitempty"`
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

// sessionPathForChat returns the path to the session JSONL file for a given chat.
// Session files contain plaintext conversation history (including user messages and
// model responses). The file is created and written by the external reasonix serve
// process, not by the bridge. Permissions are inherited from the reasonix serve
// process's umask; if tighter control is needed, reasonix serve should be configured
// to create these files with 0o600.
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
		log.Printf("state: failed to unmarshal state file %s: %v (keeping backup)", st.path, err)
		os.Rename(st.path, st.path+".bak")
		return []chatRecord{}, nil
	}
	return sf.Chats, nil
}

func (st *stateStore) upsert(rec chatRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var sf stateFile
	if b, err := os.ReadFile(st.path); err == nil {
		if err := json.Unmarshal(b, &sf); err != nil {
			log.Printf("state: failed to unmarshal state file %s: %v (keeping backup)", st.path, err)
			os.Rename(st.path, st.path+".bak")
			sf = stateFile{}
		}
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
		if err := json.Unmarshal(b, &sf); err != nil {
			log.Printf("state: failed to unmarshal state file %s: %v (keeping backup)", st.path, err)
			os.Rename(st.path, st.path+".bak")
			sf = stateFile{}
		}
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

// saveAll replaces the entire chat state with the given records.
func (st *stateStore) saveAll(records []chatRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.writeLocked(&stateFile{Chats: records})
}

// updateAll runs fn inside the state lock, reading current records, applying fn,
// and saving the result atomically.
func (st *stateStore) updateAll(fn func([]chatRecord) []chatRecord) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var sf stateFile
	if b, err := os.ReadFile(st.path); err == nil {
		if err := json.Unmarshal(b, &sf); err != nil {
			log.Printf("state: failed to unmarshal state file %s: %v (keeping backup)", st.path, err)
			os.Rename(st.path, st.path+".bak")
			sf = stateFile{}
		}
	}
	sf.Chats = fn(sf.Chats)
	return st.writeLocked(&sf)
}

func (st *stateStore) writeLocked(sf *stateFile) error {
	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	// Sync parent directory so rename is durable after a crash.
	return syncDir(filepath.Dir(st.path))
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

// chatIDsWithSessionJSONL returns chat IDs that have a non-empty session file on disk.
// Only called during startup before concurrent access begins.
func (st *stateStore) chatIDsWithSessionJSONL() []int64 {
	entries, err := os.ReadDir(st.sessionsDir())
	if err != nil {
		return nil
	}
	var ids []int64
	for _, ent := range entries {
		name := ent.Name()
		if !strings.HasSuffix(name, ".jsonl") || ent.IsDir() {
			continue
		}
		base := strings.TrimSuffix(name, ".jsonl")
		id, err := strconv.ParseInt(base, 10, 64)
		if err != nil || id == 0 {
			continue
		}
		info, err := ent.Info()
		if err != nil || info.Size() == 0 {
			continue
		}
		ids = append(ids, id)
	}
	return ids
}

// cleanupOrphanSessionArtifacts removes checkpoint/meta files left after /new
// when the session JSONL is gone.
func (st *stateStore) cleanupOrphanSessionArtifacts() {
	entries, err := os.ReadDir(st.sessionsDir())
	if err != nil {
		return
	}
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() && strings.HasSuffix(name, ".ckpt") {
			chatID := strings.TrimSuffix(name, ".ckpt")
			if _, err := os.Stat(filepath.Join(st.sessionsDir(), chatID+".jsonl")); os.IsNotExist(err) {
				_ = os.RemoveAll(filepath.Join(st.sessionsDir(), name))
			}
			continue
		}
		if strings.HasSuffix(name, ".jsonl.meta") {
			chatID := strings.TrimSuffix(name, ".jsonl.meta")
			if _, err := os.Stat(filepath.Join(st.sessionsDir(), chatID+".jsonl")); os.IsNotExist(err) {
				_ = os.Remove(filepath.Join(st.sessionsDir(), name))
			}
		}
	}
}

// sessionStats reads a Reasonix session JSONL for logging resume health.
func sessionStats(path string) (messages int, userTurns int, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	if IsEncrypted(data) {
		plain, err := Decrypt(data)
		if err != nil {
			return 0, 0, err
		}
		data = plain
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		var m struct {
			Role string `json:"role"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, 0, err
		}
		messages++
		if m.Role == "user" {
			userTurns++
		}
	}
	return messages, userTurns, nil
}