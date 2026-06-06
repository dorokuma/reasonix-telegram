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

func TestFormat_table(t *testing.T) {
	input := "| 问题 | 核心思想 |\n|---|---|\n| 百钱百鸡 | 公鸡5文、母鸡3文 |\n| 韩信点兵 | n mod 3=2 |"
	got := formatForTelegram(input)
	// First column becomes bold heading, second column is bullets
	if !stringsContains(got, "<b>百钱百鸡</b>") {
		t.Fatalf("missing row heading in %q", got)
	}
	if !stringsContains(got, "核心思想: 公鸡5文") {
		t.Fatalf("missing cell data in %q", got)
	}
}

func TestFormat_tableInsideCodeBlock(t *testing.T) {
	// Tables inside ``` fences must be left untouched.
	input := "```\n| a | b |\n|---|---|\n| 1 | 2 |\n```"
	got := formatForTelegram(input)
	if !stringsContains(got, "<pre>") {
		t.Fatalf("expected code block in %q", got)
	}
	// The pipe chars should be inside the pre block
	if stringsContains(got, "•") {
		t.Fatalf("table inside code block was converted: %q", got)
	}
}

func TestFormat_stripLatexInline(t *testing.T) {
	got := formatForTelegram("公式 \\(f-2h\\) 的结果")
	want := "公式 f-2h 的结果"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_stripLatexDisplay(t *testing.T) {
	got := formatForTelegram("\\[\\frac{n(n+1)}{2}\\]")
	// The formula content is preserved, just the delimiters are stripped
	if stringsContains(got, "\\[") || stringsContains(got, "\\]") {
		t.Fatalf("LaTeX delimiters not stripped: %q", got)
	}
	if !stringsContains(got, "\\frac") {
		t.Fatalf("formula content lost: %q", got)
	}
}

func TestFormat_latexInsideCodeBlock(t *testing.T) {
	// LaTeX inside code blocks should have delimiters stripped too
	// (code block protection runs after latex stripping)
	input := "`\\(x^2\\)`"
	got := formatForTelegram(input)
	if !stringsContains(got, "<code>") {
		t.Fatalf("expected code block in %q", got)
	}
}

func stringsContains(s, substr string) bool {
	return strings.Contains(s, substr)
}

func TestFormat_asteriskEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // expected output; "" means check no asterisks only
	}{
		{"bold_double", "**解法A（经典）**", "<b>解法A（经典）</b>"},
		{"italic_one_two", "*解法A（经典）**", "<i>解法A（经典）</i>"},
		{"bold_code_placeholder", "**`turn_done`事件处理**", "<b><code>turn_done</code>事件处理</b>"},
		{"bold_code_placeholder_mid", "第一，**`turn_done`是唯一触发终结的信号**", "第一，<b><code>turn_done</code>是唯一触发终结的信号</b>"},
		{"bold_simple_cn", "**修了两处：**", "<b>修了两处：</b>"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := formatForTelegram(tt.input)
			t.Logf("input: %q", tt.input)
			t.Logf("output: %q", got)
			if tt.want != "" && got != tt.want {
				t.Fatalf("want %q, got %q", tt.want, got)
			}
			if stringsContains(got, "**") || stringsContains(got, "*") || stringsContains(got, "&ast;") || stringsContains(got, "&#42;") {
				t.Fatalf("asterisks leaked in output: %q", got)
			}
		})
	}
}
