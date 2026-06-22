package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
)

const chatWorkdirSubdir = "chat-wd"

func (a *App) chatWorkdir() string {
	return filepath.Join(a.cfg.StateDir, chatWorkdirSubdir)
}

// workDir returns the agent's tool workspace directory.
// When WORK_DIR is set, it returns that; otherwise defaults to "/root".
func (a *App) workDir() string {
	if a.cfg.WorkDir != "" {
		return a.cfg.WorkDir
	}
	return "/root"
}

// ensureUserRulesLinked creates symlinks for AGENTS.md and REASONIX.md into /root
// so that Reasonix can read rules and write memory within its project scope.
func (a *App) ensureUserRulesLinked() error {
	a.linkUserRulesIntoWD(a.workDir())
	return nil
}

// defaultRulesFile returns the Reasonix user-global rules path.
// Official layout: ~/.config/reasonix/REASONIX.md (see Reasonix memory.ScopeUser).
// CHAT_RULES_FILE overrides when set.
func defaultRulesFile() string {
	if src := strings.TrimSpace(os.Getenv("CHAT_RULES_FILE")); src != "" {
		if st, err := os.Stat(src); err == nil && st.Mode().IsRegular() {
			return src
		}
		log.Printf("link-rules: CHAT_RULES_FILE %q not found or not a regular file, skipping", src)
		return ""
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

// linkUserRulesIntoWD symlinks rules and memory files into the given directory
// so Reasonix can read rules and write memory within its project scope.
func (a *App) linkUserRulesIntoWD(wd string) {
	// Clean stale links for files that should be symlinks, not local copies.
	// REASONIX.md is NOT cleaned — it's the memory file that reasonix may write
	// to; removing it would discard remembers.
	for _, stale := range []string{"AGENTS.md", "CLAUDE.md"} {
		if err := os.Remove(filepath.Join(wd, stale)); err != nil && !os.IsNotExist(err) {
			log.Printf("link-rules: remove stale %s: %v", stale, err)
		}
	}
	// Symlink AGENTS.md (rules, read-only source).
	if src := defaultRulesFile(); src != "" {
		link := filepath.Join(wd, "AGENTS.md")
		if err := os.Symlink(src, link); err != nil {
			log.Printf("link-rules: symlink %s -> %s: %v", link, src, err)
		}
	}
	// Symlink REASONIX.md (memory, writable — reasonix writes remembers here).
	home, _ := os.UserHomeDir()
	memDir := filepath.Join(home, ".config", "reasonix")
	memFile := filepath.Join(memDir, "REASONIX.md")
	memLink := filepath.Join(wd, "REASONIX.md")
	// Remove stale regular file so symlink can replace it.
	if st, err := os.Lstat(memLink); err == nil && st.Mode().IsRegular() {
		if err := os.Remove(memLink); err != nil {
			log.Printf("link-rules: remove stale REASONIX.md: %v", err)
		}
	} else if err != nil && !os.IsNotExist(err) {
		log.Printf("link-rules: stat REASONIX.md: %v", err)
	}
	if err := os.MkdirAll(memDir, 0o755); err == nil {
		// Ensure the target exists so the symlink isn't dangling.
		if _, err := os.Stat(memFile); os.IsNotExist(err) {
			_ = os.WriteFile(memFile, nil, 0o600)
		}
		if err := os.Symlink(memFile, memLink); err != nil {
			log.Printf("link-rules: symlink %s -> %s: %v", memLink, memFile, err)
		}
	}
}
