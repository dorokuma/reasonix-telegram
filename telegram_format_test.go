package main

import (
	"strings"
	"testing"
)

func TestFormat_plainText(t *testing.T) {
	got := formatForTelegram("Hello world")
	if got != "Hello world" {
		t.Fatalf("want %q, got %q", "Hello world", got)
	}
}

func TestFormat_bold(t *testing.T) {
	got := formatForTelegram("This is **bold** text")
	want := "This is <b>bold</b> text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_italic(t *testing.T) {
	got := formatForTelegram("This is *italic* text")
	want := "This is <i>italic</i> text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_boldAndItalic(t *testing.T) {
	got := formatForTelegram("**bold** and *italic*")
	want := "<b>bold</b> and <i>italic</i>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_inlineCode(t *testing.T) {
	got := formatForTelegram("Use `code` here")
	want := "Use <code>code</code> here"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_fencedCode(t *testing.T) {
	input := "```\nfmt.Println(\"hi\")\n```"
	got := formatForTelegram(input)
	want := "<pre>fmt.Println(\"hi\")</pre>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_fencedCodeWithLang(t *testing.T) {
	input := "```go\nfmt.Println(\"hi\")\n```"
	got := formatForTelegram(input)
	want := "<pre>fmt.Println(\"hi\")</pre>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_link(t *testing.T) {
	got := formatForTelegram("[Click here](https://example.com)")
	want := `<a href="https://example.com">Click here</a>`
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_header(t *testing.T) {
	got := formatForTelegram("# Title")
	want := "<b>Title</b>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}

	got = formatForTelegram("## Subtitle")
	want = "<b>Subtitle</b>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_strikethrough(t *testing.T) {
	got := formatForTelegram("This is ~~deleted~~ text")
	want := "This is <s>deleted</s> text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_spoiler(t *testing.T) {
	got := formatForTelegram("This is ||hidden|| text")
	want := `This is <span class="tg-spoiler">hidden</span> text`
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_blockquote(t *testing.T) {
	got := formatForTelegram("> This is a quote")
	want := "<blockquote>This is a quote</blockquote>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_htmlEscape(t *testing.T) {
	got := formatForTelegram("A < B & C > D")
	want := "A &lt; B &amp; C &gt; D"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_codePreservesSpecialChars(t *testing.T) {
	// Code blocks should not have their content HTML-escaped in a way
	// that breaks display. The pre/code tag handles the rendering.
	got := formatForTelegram("`a < b`")
	want := "<code>a &lt; b</code>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_empty(t *testing.T) {
	got := formatForTelegram("")
	if got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestFormat_nestedBoldInHeader(t *testing.T) {
	got := formatForTelegram("## **Important**")
	want := "<b>Important</b>"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_mixed(t *testing.T) {
	input := "# Welcome\n\nThis is **bold** and *italic*.\n\n```go\nfmt.Println(\"hi\")\n```\n\n> A quote\n\nMore text."
	got := formatForTelegram(input)
	if !stringsContains(got, "<b>Welcome</b>") {
		t.Fatalf("missing header in %q", got)
	}
	if !stringsContains(got, "<b>bold</b>") {
		t.Fatalf("missing bold in %q", got)
	}
	if !stringsContains(got, "<i>italic</i>") {
		t.Fatalf("missing italic in %q", got)
	}
	if !stringsContains(got, "<pre>") {
		t.Fatalf("missing code block in %q", got)
	}
	if !stringsContains(got, "<blockquote>") {
		t.Fatalf("missing blockquote in %q", got)
	}
}

func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}
