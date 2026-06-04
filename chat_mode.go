package main

import (
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