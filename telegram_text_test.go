package main

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSplitTelegramText_short(t *testing.T) {
	parts := splitTelegramText("你好", 4096)
	if len(parts) != 1 || parts[0] != "你好" {
		t.Fatalf("got %v", parts)
	}
}

func TestSplitTelegramText_multipart(t *testing.T) {
	s := strings.Repeat("啊", 5000)
	parts := splitTelegramText(s, 4096)
	if len(parts) != 2 {
		t.Fatalf("want 2 parts, got %d", len(parts))
	}
	for i, p := range parts {
		if utf8.RuneCountInString(p) > 4096 {
			t.Fatalf("part %d too long: %d runes", i, utf8.RuneCountInString(p))
		}
	}
	if parts[0]+parts[1] != s {
		t.Fatalf("lost content")
	}
}

func TestSplitTelegramText_prefersNewline(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 3000; i++ {
		b.WriteString("line\n")
	}
	s := b.String()
	parts := splitTelegramText(s, 4096)
	if len(parts) < 2 {
		t.Fatalf("expected split, got %d parts", len(parts))
	}
	if !strings.HasSuffix(parts[0], "\n") && !strings.Contains(parts[0], "line") {
		t.Fatalf("unexpected break: %q", parts[0][:min(40, len(parts[0]))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}