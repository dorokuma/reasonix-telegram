package main

import (
	"strings"
	"testing"
)

// TestAppendChunk verifies stream buffer accumulation with truncation.
func TestAppendChunk(t *testing.T) {
	var buf strings.Builder
	var truncated bool

	appendChunk(&buf, "hello ", 100, &truncated)
	appendChunk(&buf, "world", 100, &truncated)
	if buf.String() != "hello world" {
		t.Fatalf("expected 'hello world', got %q", buf.String())
	}
	if truncated {
		t.Fatal("should not be truncated")
	}

	// Test with ANSI stripping
	appendChunk(&buf, "\x1b[32m green \x1b[0m", 100, &truncated)
	if !strings.Contains(buf.String(), "green") {
		t.Fatalf("expected green text, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "\x1b") {
		t.Fatal("ANSI codes should be stripped")
	}

	// Test truncation at cap
	var buf2 strings.Builder
	var tr2 bool
	long := strings.Repeat("a", 200)
	appendChunk(&buf2, long, 50, &tr2)
	if !tr2 {
		t.Fatal("should be truncated")
	}
	if buf2.Len() > 50 {
		t.Fatalf("buffer should be capped at 50, got %d", buf2.Len())
	}
}

// TestStripANSI verifies ANSI escape sequence removal.
func TestStripANSI(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello", "hello"},
		{"\x1b[32mhello\x1b[0m", "hello"},
		{"\x1b]0;title\x07text", "text"},
		{"\x1b@", ""}, // ESC @ (single byte in range @-_)
		{"", ""},
	}
	for _, tc := range cases {
		got := stripANSI(tc.in)
		if got != tc.want {
			t.Fatalf("stripANSI(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestIsReasonixNoise verifies noise detection.
func TestIsReasonixNoise(t *testing.T) {
	noise := []string{
		"  · 7646 tok · in 7580",
		"  · codegraph: fetching runtime",
		"  ▎ thinking",
		"  ▎ reasoning",
		"❌ something failed",
		"✅ something succeeded",
		"ℹ️ info message",
		"hook blocked something",
		"exit status 1",
		"remembered 1 fact",
		"unknown ref \"ctx-2\"",
		"unknown ref 'ctx-2'",
		"[ctx] ref=ctx-1 tool=read_file (200 lines): offset 0",
		"[ctx] something something",
	}
	keep := []string{
		"hello world",
		"this is normal text",
		"  indented but not noise",
		"· not a tok line",
		"a ▎ not at start",
		"something exit status 1",
		"something remembered 1 fact",
	}
	for _, s := range noise {
		if !isReasonixNoise(s) {
			t.Fatalf("expected noise: %q", s)
		}
	}
	for _, s := range keep {
		if isReasonixNoise(s) {
			t.Fatalf("expected keep: %q", s)
		}
	}
}

// TestStripErrorLines verifies error line removal.
func TestStripErrorLines(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"hello world", "hello world"},
		{"unknown ref \"ctx-2\"", ""},
		{"prefix unknown ref 'ctx-2' suffix", ""},
		{"[ctx] ref=ctx-1 tool=read_file", ""},
		{"normal line\nunknown ref \"ctx-5\"", "normal line"},
		{"unknown ref\nnormal line", "normal line"},
		{"normal line\n[ctx] ref=ctx-1\nanother normal", "normal line\nanother normal"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		got := stripErrorLines(tc.in)
		if got != tc.want {
			t.Fatalf("stripErrorLines(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestUserFacingError verifies error message mapping.
func TestUserFacingError(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{nil, ""},
		{errPause, "本轮已暂停（达到步数上限），可再发一条消息继续"},
		{errNotReady, "Reasonix 未就绪，请稍后重试或发送 /restart"},
		{errSubmit, "提交失败：bad request"},
		{errRefused, "无法连接 Reasonix 服务"},
	}
	for _, tc := range cases {
		got := userFacingError(tc.err)
		if got != tc.want {
			t.Fatalf("userFacingError(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// Sentinel errors for userFacingError tests.
var (
	errPause    = &testErr{"paused after 10 steps"}
	errNotReady = &testErr{"reasonix serve not ready on port 9999"}
	errSubmit   = &testErr{"submit: bad request"}
	errRefused  = &testErr{"connection refused"}
)

type testErr struct{ msg string }

func (e *testErr) Error() string { return e.msg }
