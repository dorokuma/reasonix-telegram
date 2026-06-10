package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

const chatWorkdirSubdir = "chat-wd"

// chatReasonixToml is written into the dedicated chat workdir on every bridge
// start. Only technical lockdown (tools/plugins/LSP/codegraph) — no system_prompt,
// persona, or agent behavior overrides; the user's own rules apply via Reasonix.
const chatReasonixToml = `# reasonix-telegram: tool lockdown only (managed by the bridge)
[agent]
auto_plan = "off"

[tools]
enabled = ["none"]

[lsp]
enabled = false

[codegraph]
enabled = false
auto_install = false
`

func (a *App) chatWorkdir() string {
	return filepath.Join(a.cfg.StateDir, chatWorkdirSubdir)
}

// ensureChatWorkdir prepares the per-chat reasonix workdir and writes a
// per-mode reasonix.toml that overrides the global config (reasonix.toml
// in the cwd wins over ~/.config/reasonix/config.toml). Without this, the
// chat-mode tool lockdown defined in reasonixTomlContent() is dead code.
func (a *App) ensureChatWorkdir() error {
	wd := a.chatWorkdir()
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(wd, "reasonix.toml"), []byte(chatReasonixToml), 0o644); err != nil {
		return err
	}
	a.linkUserRulesIntoChatWD(wd)
	return nil
}

// defaultRulesFile returns the Reasonix user-global rules path.
// Official layout: ~/.config/reasonix/REASONIX.md (see Reasonix memory.ScopeUser).
// CHAT_RULES_FILE overrides when set.
func defaultRulesFile() string {
	if src := strings.TrimSpace(os.Getenv("CHAT_RULES_FILE")); src != "" {
		return src
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	path := filepath.Join(home, ".config", "reasonix", "AGENTS.md")
	if st, err := os.Stat(path); err == nil && !st.IsDir() {
		return path
	}
	return ""
}

// linkUserRulesIntoChatWD symlinks rules and memory files into chat-wd
// so Reasonix can read rules and write memory within its project scope.
func (a *App) linkUserRulesIntoChatWD(wd string) {
	// Clean stale links for files that should be symlinks, not local copies.
	// REASONIX.md is NOT cleaned — it's the memory file that reasonix may write
	// to; removing it would discard remembers.
	for _, stale := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.Remove(filepath.Join(wd, stale)); err != nil && !os.IsNotExist(err) {
			log.Printf("chat-wd: remove stale %s: %v", stale, err)
		}
	}
	// Symlink AGENTS.md (rules, read-only source).
	if src := defaultRulesFile(); src != "" {
		link := filepath.Join(wd, "AGENTS.md")
		if err := os.Symlink(src, link); err != nil {
			log.Printf("chat-wd: symlink %s -> %s: %v", link, src, err)
		}
	}
	// Symlink REASONIX.md (memory, writable — reasonix writes remembers here).
	home, _ := os.UserHomeDir()
	memDir := filepath.Join(home, ".config", "reasonix")
	memFile := filepath.Join(memDir, "REASONIX.md")
	if err := os.MkdirAll(memDir, 0o755); err == nil {
		// Ensure the target exists so the symlink isn't dangling.
		if _, err := os.Stat(memFile); os.IsNotExist(err) {
			_ = os.WriteFile(memFile, nil, 0o600)
		}
		memLink := filepath.Join(wd, "REASONIX.md")
		if err := os.Symlink(memFile, memLink); err != nil {
			log.Printf("chat-wd: symlink %s -> %s: %v", memLink, memFile, err)
		}
	}
}