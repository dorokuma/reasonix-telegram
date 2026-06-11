package main

import (
	"testing"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// TestAllowed verifies the ALLOWED_USERS access control.
func TestAllowed(t *testing.T) {
	app := &App{cfg: Config{}}
	// Empty list = allow all
	if !app.allowed(&tgbotapi.User{ID: 42}) {
		t.Fatal("empty ALLOWED_USERS should allow anyone")
	}
	if !app.allowed(&tgbotapi.User{ID: 0}) {
		t.Fatal("empty ALLOWED_USERS should allow even ID 0")
	}

	// Non-empty list = strict check
	app.cfg.AllowedUsers = []int64{42, 99}
	if !app.allowed(&tgbotapi.User{ID: 42}) {
		t.Fatal("user 42 should be allowed")
	}
	if !app.allowed(&tgbotapi.User{ID: 99}) {
		t.Fatal("user 99 should be allowed")
	}
	if app.allowed(&tgbotapi.User{ID: 1}) {
		t.Fatal("user 1 should be denied")
	}
}

// TestModeLabel verifies mode label formatting.
func TestModeLabel(t *testing.T) {
	app := &App{}
	app.setMode(ModeChat)
	if app.modeLabel() != "💬 聊天模式" {
		t.Fatalf("chat mode label: got %q", app.modeLabel())
	}
	app.setMode(ModeTool)
	if app.modeLabel() != "⌨️ 编程模式" {
		t.Fatalf("tool mode label: got %q", app.modeLabel())
	}
}

// TestGetOrCreateSession verifies session creation and reuse.
func TestGetOrCreateSession(t *testing.T) {
	dir := t.TempDir()
	st, err := newStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:   Config{StateDir: dir},
		state: st,
		sess:  map[int64]*session{},
	}
	app.setMode(ModeChat)
	_ = app.ensureChatWorkdir()

	s1 := app.getOrCreateSession(123)
	if s1 == nil {
		t.Fatal("got nil session")
	}
	if s1.servePort == 0 {
		t.Fatal("session should have a port assigned")
	}

	// Same chat returns same session
	s2 := app.getOrCreateSession(123)
	if s1 != s2 {
		t.Fatal("same chat should return same session pointer")
	}

	// Different chat returns different session
	s3 := app.getOrCreateSession(456)
	if s1 == s3 {
		t.Fatal("different chat should return different session")
	}
}

// TestResetReasonixSession verifies session reset clears state.
func TestResetReasonixSession(t *testing.T) {
	dir := t.TempDir()
	st, err := newStateStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		cfg:   Config{StateDir: dir},
		state: st,
		sess:  map[int64]*session{},
	}
	app.setMode(ModeChat)

	s := app.getOrCreateSession(789)
	s.mu.Lock()
	s.cumPrompt = 100
	s.cumCompletion = 50
	s.cumTotal = 150
	s.cumCost = 0.05
	s.cumCurrency = "USD"
	s.mu.Unlock()

	app.resetReasonixSession(789)

	s.mu.Lock()
	if s.cumTotal != 0 {
		t.Fatalf("expected cumTotal=0 after reset, got %d", s.cumTotal)
	}
	if s.cumCost != 0 {
		t.Fatalf("expected cumCost=0 after reset, got %f", s.cumCost)
	}
	s.mu.Unlock()
}
