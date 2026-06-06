package main

import (
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
auto_plan = "ask"

[lsp]
enabled = true

[codegraph]
enabled = true
auto_install = true

[permissions]
mode = "allow"

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

// defaultReasonixTomlPath returns the path to the default reasonix.toml template.
func (a *App) ensureChatWorkdir() error {
	wd := a.chatWorkdir()
	if err := os.MkdirAll(wd, 0o755); err != nil {
		return err
	}
	// Don't write reasonix.toml — serve reads global ~/.config/reasonix/config.toml.
	// User edits config there directly; no hardcoded overrides in bridge code.
	_ = os.Remove(filepath.Join(wd, "reasonix.toml"))
	a.linkUserRulesIntoChatWD(wd)
	return nil
}

// linkUserRulesIntoChatWD symlinks the user's existing rules file into chat-wd so
// Reasonix memory load picks them up. Does not write or edit rule content.
// CHAT_RULES_FILE env overrides; else tries /root/AGENTS.md then /root/REASONIX.md.
func (a *App) linkUserRulesIntoChatWD(wd string) {
	src := strings.TrimSpace(os.Getenv("CHAT_RULES_FILE"))
	if src == "" {
		for _, c := range []string{"/root/AGENTS.md", "/root/REASONIX.md"} {
			if st, err := os.Stat(c); err == nil && !st.IsDir() {
				src = c
				break
			}
		}
	}
	if src == "" {
		return
	}
	link := filepath.Join(wd, filepath.Base(src))
	_ = os.Remove(link)
	_ = os.Symlink(src, link)
}