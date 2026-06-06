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
auto_plan = "off"

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