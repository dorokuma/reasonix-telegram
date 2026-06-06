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