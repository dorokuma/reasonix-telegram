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
			name:   "reasoning only",
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
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := reasoningFallbackBody(tc.in)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if got != tc.want {
				t.Fatalf("body = %q, want %q", got, tc.want)
			}
		})
	}
}