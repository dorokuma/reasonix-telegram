package main

import "testing"

func TestDraftHadPreview(t *testing.T) {
	t.Parallel()
	if draftHadPreview("") {
		t.Fatal("empty preview should be false")
	}
	if draftHadPreview("   ") {
		t.Fatal("whitespace preview should be false")
	}
	if !draftHadPreview("hello") {
		t.Fatal("non-empty preview should be true")
	}
}

func TestDraftNeedsCleanup(t *testing.T) {
	t.Parallel()
	// With sendRichMessageDraft, no manual cleanup is needed — the final
	// sendRichMessage replaces the draft automatically.
	if draftNeedsCleanup(false, false, "") {
		t.Fatal("should be false")
	}
	if draftNeedsCleanup(false, true, "") {
		t.Fatal("should be false with Rich Messages")
	}
	if draftNeedsCleanup(true, false, "") {
		t.Fatal("should be false with Rich Messages")
	}
}
