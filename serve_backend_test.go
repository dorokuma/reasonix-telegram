package main

import "testing"

func TestShouldFlushReasoningFallback(t *testing.T) {
	t.Parallel()
	if shouldFlushReasoningFallback(true, false) {
		t.Fatal("text deltas present: should not flush reasoning")
	}
	if shouldFlushReasoningFallback(false, true) {
		t.Fatal("tool dispatched: should not flush reasoning")
	}
	if !shouldFlushReasoningFallback(false, false) {
		t.Fatal("reasoning-only turn: should flush reasoning")
	}
}

func TestReasoningFallbackBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		in     string
		wantOK bool
		want   string
	}{
		{
			name:   "chinese response",
			in:     "爸爸让我读规则，找出已经存在的相关规则。",
			wantOK: true,
			want:   "爸爸让我读规则，找出已经存在的相关规则。",
		},
		{
			name:   "strips think blocks",
			in:     "<think>hidden</think>可见结论",
			wantOK: true,
			want:   "可见结论",
		},
		{
			name:   "empty",
			in:     "   ",
			wantOK: false,
		},
		{
			name:   "silence narration",
			in:     "silent",
			wantOK: false,
		},
		{
			name:   "english thinking rejected",
			in:     "The user wants me to find the rules file and look for existing rules.",
			wantOK: false,
		},
		{
			name:   "chinese thinking opener rejected",
			in:     "让我先看看代码里有没有相关配置。",
			wantOK: false,
		},
		{
			name:   "chinese thinking opener 2 rejected",
			in:     "首先分析一下用户的需求。",
			wantOK: false,
		},
		{
			name:   "chinese thinking 我需要 rejected",
			in:     "我需要先确认一下当前的工作目录。",
			wantOK: false,
		},
		{
			name:   "mixed eng thinking rejected",
			in:     "Let me first check the config file.\n看看有没有相关设置。",
			wantOK: false,
		},
		{
			name:   "chinese response with code keeps",
			in:     "已找到问题，在 main.go 第 42 行。",
			wantOK: true,
			want:   "已找到问题，在 main.go 第 42 行。",
		},
		{
			name:   "english thinking multi para",
			in:     "The user is asking about X.\nLet me look at the code first.\nI need to find the relevant file.",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := reasoningFallbackBody(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if tc.wantOK && got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsPureEnglish(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"The user wants me to check the config.", true},
		{"Let me look at the code first.", true},
		{"已找到问题", false},
		{"mixed English 和中文", false},
		{"OK", false}, // too short
		{"", false},
	}
	for _, tc := range cases {
		got := isPureEnglish(tc.in)
		if got != tc.want {
			t.Fatalf("isPureEnglish(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestHasCJK(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"Hello world", false},
		{"中文测试", true},
		{"mixed English 中文", true},
		{"", false},
	}
	for _, tc := range cases {
		got := hasCJK(tc.in)
		if got != tc.want {
			t.Fatalf("hasCJK(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestStripThinkBlocksStateful(t *testing.T) {
	t.Parallel()

	t.Run("single chunk complete block", func(t *testing.T) {
		var inThinking bool
		got := stripThinkBlocksStateful("before <thinking>hidden</thinking> after", &inThinking)
		if got != "before  after" {
			t.Fatalf("got %q, want %q", got, "before  after")
		}
		if inThinking {
			t.Fatal("inThinking should be false after complete block")
		}
	})

	t.Run("cross chunk open in first", func(t *testing.T) {
		var inThinking bool
		got1 := stripThinkBlocksStateful("before <thinking>hidden content ", &inThinking)
		if got1 != "before " {
			t.Fatalf("chunk1: got %q, want %q", got1, "before ")
		}
		if !inThinking {
			t.Fatal("inThinking should be true after open tag")
		}

		got2 := stripThinkBlocksStateful("more hidden text", &inThinking)
		if got2 != "" {
			t.Fatalf("chunk2: got %q, want empty (all thinking)", got2)
		}
		if !inThinking {
			t.Fatal("inThinking should still be true")
		}

		got3 := stripThinkBlocksStateful("</thinking> after", &inThinking)
		if got3 != " after" {
			t.Fatalf("chunk3: got %q, want %q", got3, " after")
		}
		if inThinking {
			t.Fatal("inThinking should be false after close tag")
		}
	})

	t.Run("close tag alone discarded", func(t *testing.T) {
		var inThinking bool
		got := stripThinkBlocksStateful("</thinking> hello", &inThinking)
		// Not in a thinking block, so the closing tag is just text
		if got != "</thinking> hello" {
			t.Fatalf("got %q, want %q", got, "</thinking> hello")
		}
	})

	t.Run("multiple complete blocks in one chunk", func(t *testing.T) {
		var inThinking bool
		got := stripThinkBlocksStateful("a<think>b</think>c<think>d</think>e", &inThinking)
		if got != "ace" {
			t.Fatalf("got %q, want %q", got, "ace")
		}
	})

	t.Run("empty chunk", func(t *testing.T) {
		var inThinking bool
		got := stripThinkBlocksStateful("", &inThinking)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("ansi inside thinking still stripped", func(t *testing.T) {
		var inThinking bool
		// ANSI is stripped BEFORE calling stripThinkBlocksStateful
		got := stripThinkBlocksStateful("visible<thinking>\x1b[32mhidden\x1b[0m</thinking>end", &inThinking)
		if got != "visibleend" {
			t.Fatalf("got %q, want %q", got, "visibleend")
		}
	})
}

func TestIsLikelyThinking(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// English thinking
		{"The user wants me to check the config.", true},
		{"Let me look at the code.", true},
		// Chinese thinking openers
		{"让我先看看代码。", true},
		{"首先分析一下需求。", true},
		{"我需要先确认一下。", true},
		{"我们先看看这个文件。", true},
		// Chinese responses
		{"已找到问题所在。", false},
		{"已修改完成。", false},
		{"爸爸让我读规则，找出已经存在的相关规则。", false},
		// Mixed with thinking opener
		{"然后我再检查一下。", false},
		// English thinking openers
		{"Let me check the config first.", true},
		{"I need to find the relevant file.", true},
		{"The user is asking about X.", true},
		// Mixed: English opener + Chinese tail
		{"Let me first check the config file.\n看看有没有相关设置。", true},
	}
	for _, tc := range cases {
		got := isLikelyThinking(tc.in)
		if got != tc.want {
			t.Fatalf("isLikelyThinking(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}