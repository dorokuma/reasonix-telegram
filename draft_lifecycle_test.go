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
	if draftNeedsCleanup(false, false, "") {
		t.Fatal("no draft state")
	}
	if !draftNeedsCleanup(false, true, "") {
		t.Fatal("liveDraftEver alone should need cleanup")
	}
	if !draftNeedsCleanup(true, false, "") {
		t.Fatal("draftShown alone should need cleanup")
	}
	if draftNeedsCleanup(false, false, "preview") {
		t.Fatal("edit preview alone must not trigger native draft dismiss")
	}
}