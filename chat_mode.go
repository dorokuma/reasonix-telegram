package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

const chatWorkdirSubdir = "chat-wd"

// reasonixTomlContent returns the reasonix.toml written into chat-wd.
// In chat mode: lock down tools/LSP/codegraph.
// In tool mode: open config with permissions=allow + codegraph/LSP on.
func (a *App) reasonixTomlContent() string {
	if a.getMode() == ModeTool {
		return `# reasonix-telegram: tool mode (managed by the bridge)
[agent]
auto_plan = "off"

[lsp]
enabled = true

[codegraph]
enabled = true
auto_install = true

[permissions]
mode = "allow"

[tools]
enabled = []   # all built-ins; do not inherit a partial global whitelist

[tools.search]
engine = "rtk"

[sandbox]
workspace_root = "/"
allow_write = ["/**"]
bash = "off"
network = true
`
	}
	return `# reasonix-telegram: chat mode — tool lockdown (managed by the bridge)
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
}

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
	// Write per-mode reasonix.toml. reasonix resolves config in this order:
	//   flag > ./reasonix.toml > ~/.config/reasonix/config.toml > defaults
	// Without a local toml the global config (which has the full toolset
	// enabled) wins and the chat-mode lockdown in reasonixTomlContent()
	// is dead code. Writing it here makes chat-mode truly tool-free.
	tomlPath := filepath.Join(wd, "reasonix.toml")
	if err := os.WriteFile(tomlPath, []byte(a.reasonixTomlContent()), 0o644); err != nil {
		return err
	}
	a.linkUserRulesIntoChatWD(wd)
	return nil
}

// linkUserRulesIntoChatWD symlinks the user's existing rules file into chat-wd so
// Reasonix memory load picks them up. Does not write or edit rule content.
// CHAT_RULES_FILE env overrides; else ~/.config/reasonix/REASONIX.md (same as TUI).
func (a *App) linkUserRulesIntoChatWD(wd string) {
	src := strings.TrimSpace(os.Getenv("CHAT_RULES_FILE"))
	if src == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			candidate := filepath.Join(dir, "reasonix", "REASONIX.md")
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				src = candidate
			}
		}
	}
	if src == "" {
		return
	}
	link := filepath.Join(wd, filepath.Base(src))
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		log.Printf("remove old rules link %s: %v", link, err)
	}
	if err := os.Symlink(src, link); err != nil {
		log.Printf("symlink rules %s -> %s: %v", link, src, err)
	}

	// Symlink REASONIX.md (memory, writable — reasonix writes remembers here).
	home, _ := os.UserHomeDir()
	memDir := filepath.Join(home, ".config", "reasonix")
	memFile := filepath.Join(memDir, "REASONIX.md")
	if err := os.MkdirAll(memDir, 0o755); err == nil {
		if _, err := os.Stat(memFile); os.IsNotExist(err) {
			_ = os.WriteFile(memFile, nil, 0o600)
		}
		memLink := filepath.Join(wd, "REASONIX.md")
		if err := os.Symlink(memFile, memLink); err != nil {
			log.Printf("chat-wd: symlink %s -> %s: %v", memLink, memFile, err)
		}
	}
}
