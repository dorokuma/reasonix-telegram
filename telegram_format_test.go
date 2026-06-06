// reasonix-telegram: tests for the MDV2 markdown converter
// (telegram_format.go). These tests pin the expected Telegram MarkdownV2
// output, ported from the equivalent Hermes test cases in
// hermes-agent/tests/gateway/test_telegram_format.py.
package main

import (
	"strings"
	"testing"
)

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// -----------------------------------------------------------------------
// Plain text & basic
// -----------------------------------------------------------------------

func TestFormat_empty(t *testing.T) {
	if got := formatForTelegram(""); got != "" {
		t.Fatalf("want empty, got %q", got)
	}
}

func TestFormat_plainText(t *testing.T) {
	if got := formatForTelegram("Hello world"); got != "Hello world" {
		t.Fatalf("want %q, got %q", "Hello world", got)
	}
}

func TestFormat_htmlEscape(t *testing.T) {
	// <, >, & are NOT special in MDV2, only some chars get backslash-escaped.
	// 'A' is plain, ' < ' stays, ' > ' becomes '\>' (MDV2 special), '&' stays.
	got := formatForTelegram("A < B & C > D")
	want := "A < B & C \\> D"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_plainTextSpecialsEscaped(t *testing.T) {
	got := formatForTelegram("Price is $5.00!")
	want := "Price is $5\\.00\\!"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_parenAndBang(t *testing.T) {
	got := formatForTelegram("Hello (World)!")
	want := "Hello \\(World\\)\\!"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// -----------------------------------------------------------------------
// Code blocks & inline code (must not be escaped)
// -----------------------------------------------------------------------

func TestFormat_inlineCode(t *testing.T) {
	got := formatForTelegram("Use `code` here")
	want := "Use `code` here"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_fencedCode(t *testing.T) {
	got := formatForTelegram("```\nfmt.Println(\"hi\")\n```")
	want := "```\nfmt.Println(\"hi\")\n```"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_fencedCodeWithLang(t *testing.T) {
	got := formatForTelegram("```go\nfmt.Println(\"hi\")\n```")
	want := "```go\nfmt.Println(\"hi\")\n```"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_fencedCodeSpecialCharsNotEscaped(t *testing.T) {
	got := formatForTelegram("```\nif (x > 0) { return !x; }\n```")
	want := "```\nif (x > 0) { return !x; }\n```"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_inlineCodeSpecialCharsNotEscaped(t *testing.T) {
	got := formatForTelegram("Run `rm -rf ./*` carefully")
	want := "Run `rm -rf ./*` carefully"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_codeBlockPreservesSpecialChars(t *testing.T) {
	got := formatForTelegram("`a < b`")
	want := "`a < b`"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// -----------------------------------------------------------------------
// Bold & italic
// -----------------------------------------------------------------------

func TestFormat_bold(t *testing.T) {
	got := formatForTelegram("This is **bold** text")
	want := "This is *bold* text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_italic(t *testing.T) {
	got := formatForTelegram("This is *italic* text")
	want := "This is _italic_ text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_boldAndItalic(t *testing.T) {
	got := formatForTelegram("**bold** and *italic*")
	want := "*bold* and _italic_"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_boldWithSpecialChars(t *testing.T) {
	got := formatForTelegram("**hello.world!**")
	want := "*hello\\.world\\!*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_italicWithSpecialChars(t *testing.T) {
	got := formatForTelegram("*hello.world*")
	want := "_hello\\.world_"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_boldChineseWithColon(t *testing.T) {
	got := formatForTelegram("**修了两处：**")
	want := "*修了两处：*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_asteriskEdgeCases(t *testing.T) {
	// *解法A（经典）** — the first * opens italic, the second * closes it.
	// The remaining ** collapses to * which gets MDV2-escaped as \*.
	got := formatForTelegram("*解法A（经典）**")
	want := "_解法A（经典）_\\*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// -----------------------------------------------------------------------
// Headers
// -----------------------------------------------------------------------

func TestFormat_header(t *testing.T) {
	got := formatForTelegram("# Title")
	want := "*Title*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
	got = formatForTelegram("## Subtitle")
	want = "*Subtitle*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_nestedBoldInHeader(t *testing.T) {
	got := formatForTelegram("## **Important**")
	want := "*Important*"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_headerWithSpecialChars(t *testing.T) {
	got := formatForTelegram("# Hello (World)!")
	// header *Hello \(World\)!* — content escaped, wrapped in * *
	if got != "*Hello \\(World\\)\\!*" {
		t.Fatalf("got %q", got)
	}
}

// -----------------------------------------------------------------------
// Strikethrough & spoiler
// -----------------------------------------------------------------------

func TestFormat_strikethrough(t *testing.T) {
	got := formatForTelegram("This is ~~deleted~~ text")
	want := "This is ~deleted~ text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_spoiler(t *testing.T) {
	got := formatForTelegram("This is ||hidden|| text")
	want := "This is ||hidden|| text"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// -----------------------------------------------------------------------
// Link (kept as MDV2 [text](url) syntax; the URL is in the placeholder
// so the plain-text [ ] chars are not escaped)
// -----------------------------------------------------------------------

func TestFormat_link(t *testing.T) {
	got := formatForTelegram("[Click here](https://example.com)")
	want := "[Click here](https://example.com)"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_linkParenInURL(t *testing.T) {
	got := formatForTelegram("[wiki](https://en.wikipedia.org/wiki/Go_(programming_language))")
	// The ')' inside the URL is escaped per MDV2 spec.
	if !contains(got, "https://en.wikipedia.org/wiki/Go_\\(programming_language\\)") {
		t.Fatalf("expected paren-escaped URL in %q", got)
	}
}

// -----------------------------------------------------------------------
// Blockquote
// -----------------------------------------------------------------------

func TestFormat_blockquote(t *testing.T) {
	got := formatForTelegram("> This is a quote")
	want := "> This is a quote"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

// -----------------------------------------------------------------------
// Mixed (smoke test — covers multiple conversions in one input)
// -----------------------------------------------------------------------

func TestFormat_mixed(t *testing.T) {
	input := "# Welcome\n\nThis is **bold** and *italic*.\n\n```go\nfmt.Println(\"hi\")\n```\n\n> A quote\n\nMore text."
	got := formatForTelegram(input)
	if !contains(got, "*Welcome*") {
		t.Fatalf("missing header in %q", got)
	}
	if !contains(got, "*bold*") {
		t.Fatalf("missing bold in %q", got)
	}
	if !contains(got, "_italic_") {
		t.Fatalf("missing italic in %q", got)
	}
	if !contains(got, "```") {
		t.Fatalf("missing code block in %q", got)
	}
	if !contains(got, "> A quote") {
		t.Fatalf("missing blockquote in %q", got)
	}
}

func TestFormat_realisticMCPMessage(t *testing.T) {
	content := "🔄 **MCP Servers Reloaded**\n" +
		"♻️ Reconnected: agent_one, tool[beta]\n" +
		"➕ Added: alpha*prod"
	got := formatForTelegram(content)
	if !contains(got, "*MCP Servers Reloaded*") {
		t.Fatalf("missing bold title in %q", got)
	}
	if !contains(got, "agent\\_one") {
		t.Fatalf("missing escaped underscore in %q", got)
	}
	if !contains(got, "tool\\[beta\\]") {
		t.Fatalf("missing escaped brackets in %q", got)
	}
	if !contains(got, "alpha\\*prod") {
		t.Fatalf("missing escaped asterisk in %q", got)
	}
}

// -----------------------------------------------------------------------
// Tables (GFM → bullet groups)
// -----------------------------------------------------------------------

func TestFormat_table(t *testing.T) {
	input := "| 问题 | 核心思想 |\n|---|---|\n| 百钱百鸡 | 公鸡5文、母鸡3文 |\n| 韩信点兵 | n mod 3=2 |"
	got := formatForTelegram(input)
	if !contains(got, "*百钱百鸡*") {
		t.Fatalf("missing row heading in %q", got)
	}
	if !contains(got, "• 核心思想: 公鸡5文") {
		t.Fatalf("missing cell data in %q", got)
	}
}

func TestFormat_tableInsideCodeBlock(t *testing.T) {
	// Tables inside ``` fences must be left untouched.
	input := "```\n| a | b |\n|---|---|\n| 1 | 2 |\n```"
	got := formatForTelegram(input)
	if !contains(got, "```") {
		t.Fatalf("expected code block in %q", got)
	}
	// The pipe chars should still be in the original form (no bullet conversion).
	if contains(got, "•") {
		t.Fatalf("table inside code block was converted: %q", got)
	}
}

// -----------------------------------------------------------------------
// LaTeX delimiter stripping (Reasonix-specific)
// -----------------------------------------------------------------------

func TestFormat_stripLatexInline(t *testing.T) {
	got := formatForTelegram("公式 \\(f-2h\\) 的结果")
	want := "公式 f\\-2h 的结果"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestFormat_stripLatexDisplay(t *testing.T) {
	got := formatForTelegram("\\[\\frac{n(n+1)}{2}\\]")
	if contains(got, "\\[") || contains(got, "\\]") {
		t.Fatalf("LaTeX delimiters not stripped: %q", got)
	}
	if !contains(got, "\\frac") {
		t.Fatalf("formula content lost: %q", got)
	}
}

// -----------------------------------------------------------------------
// Code block preserves content (Hermes parity tests)
// -----------------------------------------------------------------------

func TestFormat_fencedCodeBlockPreserved(t *testing.T) {
	text := "Before\n```python\nprint('hello')\n```\nAfter"
	got := formatForTelegram(text)
	if !contains(got, "```python\nprint('hello')\n```") {
		t.Fatalf("code block content not preserved: %q", got)
	}
	if !contains(got, "After") {
		t.Fatalf("trailing text missing: %q", got)
	}
}

func TestFormat_multipleCodeBlocks(t *testing.T) {
	text := "```\nblock1\n```\ntext\n```\nblock2\n```"
	got := formatForTelegram(text)
	if !contains(got, "block1") {
		t.Fatalf("missing block1: %q", got)
	}
	if !contains(got, "block2") {
		t.Fatalf("missing block2: %q", got)
	}
	if !contains(got, "text") {
		t.Fatalf("missing inter-block text: %q", got)
	}
}

func TestFormat_inlineCodeNoDoubleEscape(t *testing.T) {
	// `\\server\share` (one literal \) → in MDV2: `\\server\share` (each \
	// escaped once). Source is r"Use `\\server\share`" which is the literal
	// string  Use `\\server\share` (Go: 2 backslashes between `\\server` and
	// `share`).
	text := "Use `\\\\server\\share`"
	got := formatForTelegram(text)
	// The expected is each \ → \\ in the inline code span.
	if !contains(got, "`\\\\\\\\server\\\\share`") {
		t.Fatalf("expected escaped backslashes in %q", got)
	}
}

// -----------------------------------------------------------------------
// Hook message stripping (Reasonix-specific)
// -----------------------------------------------------------------------

func TestFormat_stripHookMessages(t *testing.T) {
	in := "before\nblocked: hook ran something\nafter\nPlease add 'rtk' prefix please\nend"
	got := formatForTelegram(in)
	if contains(got, "blocked: hook") {
		t.Fatalf("hook line not stripped: %q", got)
	}
	if contains(got, "Please add 'rtk'") {
		t.Fatalf("rtk hint not stripped: %q", got)
	}
	if !contains(got, "before") || !contains(got, "after") || !contains(got, "end") {
		t.Fatalf("non-hook content lost: %q", got)
	}
}

// -----------------------------------------------------------------------
// _stripMdv2 fallback helper
// -----------------------------------------------------------------------

func TestStripMdv2(t *testing.T) {
	// Inverse of _escapeMdv2: should remove \ from any escaped MDV2 char.
	in := "Hello \\(World\\)\\! and \\. \\* \\_test\\|"
	got := _stripMdv2(in)
	want := "Hello (World)! and . * _test|"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestStripMdv2_formattingMarkers(t *testing.T) {
	// Should also strip bold/italic/strike/spoiler markers.
	in := "*bold* and _italic_ and ~strike~ and ||spoiler||"
	got := _stripMdv2(in)
	want := "bold and italic and strike and spoiler"
	if got != want {
		t.Fatalf("want %q, got %q", want, got)
	}
}

func TestStripMdv2_preservesSnakeCase(t *testing.T) {
	// _my_var_ should NOT be stripped because underscores are inside
	// word characters (Hermes uses \b boundaries).
	in := "keep my_variable_name verbatim"
	got := _stripMdv2(in)
	if got != "keep my_variable_name verbatim" {
		t.Fatalf("snake_case mangled: %q", got)
	}
}
