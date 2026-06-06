package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCleanupOrphanSessionArtifacts(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "99.jsonl.meta"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(sessions, "99.ckpt"), 0o755); err != nil {
		t.Fatal(err)
	}
	st := &stateStore{dir: dir, path: filepath.Join(dir, "state.json")}
	st.cleanupOrphanSessionArtifacts()
	if _, err := os.Stat(filepath.Join(sessions, "99.jsonl.meta")); !os.IsNotExist(err) {
		t.Fatal("meta should be removed")
	}
	if _, err := os.Stat(filepath.Join(sessions, "99.ckpt")); !os.IsNotExist(err) {
		t.Fatal("ckpt should be removed")
	}
}

func TestChatIDsWithSessionJSONL(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "42.jsonl"), []byte(`{"role":"user"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "empty.jsonl"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	st := &stateStore{dir: dir, path: filepath.Join(dir, "state.json")}
	ids := st.chatIDsWithSessionJSONL()
	if len(ids) != 1 || ids[0] != 42 {
		t.Fatalf("ids = %v, want [42]", ids)
	}
}