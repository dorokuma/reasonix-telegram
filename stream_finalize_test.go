package main

import "testing"

func TestStreamFinalizeBody_prefersBuffer(t *testing.T) {
	got := streamFinalizeBody("  hello  ", "world")
	if got != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestStreamFinalizeBody_fallsBackToDraft(t *testing.T) {
	got := streamFinalizeBody("", "  draft preview  ")
	if got != "draft preview" {
		t.Fatalf("got %q", got)
	}
}

func TestStreamFinalizeBody_empty(t *testing.T) {
	if streamFinalizeBody("", "") != "" {
		t.Fatal("want empty")
	}
}