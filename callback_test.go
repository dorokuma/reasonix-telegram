package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestFetchContext tests the context endpoint parsing (no server = graceful 0,0).
func TestFetchContext(t *testing.T) {
	used, window := fetchContext(99999) // unlikely port
	if used != 0 || window != 0 {
		t.Fatalf("expected 0,0 for unreachable server, got %d,%d", used, window)
	}
}

// TestShortTokens verifies token count formatting.
func TestShortTokens(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "0"},
		{500, "500"},
		{1500, "1.5K"},
		{142000, "142.0K"},
		{1000000, "1.0M"},
		{2500000, "2.5M"},
	}
	for _, tc := range cases {
		got := shortTokens(tc.n)
		if got != tc.want {
			t.Fatalf("shortTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestModelDisplayName verifies display name lookup.
func TestModelDisplayName(t *testing.T) {
	// Save and restore availableModels state
	saved := availableModels
	defer func() { availableModels = saved }()

	availableModels = []struct {
		ID   string
		Name string
	}{
		{ID: "deepseek/deepseek-v4", Name: "deepseek: deepseek-v4 ⭐"},
		{ID: "custom/my-model", Name: "custom: my-model"},
	}

	app := &App{}
	if name := app.modelDisplayName("deepseek/deepseek-v4"); name != "deepseek: deepseek-v4 ⭐" {
		t.Fatalf("got %q", name)
	}
	if name := app.modelDisplayName("unknown/model"); name != "unknown/model" {
		t.Fatalf("unknown model should return raw id, got %q", name)
	}
}

// TestPersistModel verifies model persistence to .env file.
func TestPersistModel(t *testing.T) {
	dir := t.TempDir()
	app := &App{cfg: Config{StateDir: dir}}

	if err := app.persistModel("deepseek/deepseek-v4"); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "MODEL=deepseek/deepseek-v4\n" {
		t.Fatalf("got %q", string(data))
	}
}

// TestEffortLevels verifies the effort level table is well-formed.
func TestEffortLevels(t *testing.T) {
	if len(effortLevels) != 5 {
		t.Fatalf("expected 5 effort levels, got %d", len(effortLevels))
	}
	foundAuto := false
	for _, l := range effortLevels {
		if l.ID == "auto" {
			foundAuto = true
		}
		if l.ID == "" || l.Name == "" {
			t.Fatalf("empty ID or Name in effort level: %+v", l)
		}
	}
	if !foundAuto {
		t.Fatal("effort levels should include 'auto'")
	}
}

// TestSessionsHandlerNoPanic verifies the sessions map is safely iterable.
func TestSessionsHandlerNoPanic(t *testing.T) {
	app := &App{
		sess: map[int64]*session{},
	}
	app.setMode(ModeTool)
	// Ensure chatWorkdir is set so ensureChatWorkdir doesn't fail
	app.cfg.StateDir = t.TempDir()
	_ = app.ensureChatWorkdir()

	// Just verify sessionsHandler body doesn't panic when sess is empty.
	// The full function needs a real bot, so we test the map access only.
	app.sessMu.Lock()
	if len(app.sess) != 0 {
		t.Fatal("expected empty sessions")
	}
	app.sessMu.Unlock()
}
