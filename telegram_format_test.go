package main

import (
	"strings"
	"testing"
)

func TestFormatMessage_BashCodeBlock(t *testing.T) {
	// Fenced bash code block should be protected: backticks and backslashes inside preserved
	in := "Run this:\n```bash\ncp source target\n```\nDone."
	got := formatMessage(in)
	// The fenced block should survive as a placeholder-restored region
	if !strings.Contains(got, "```") {
		t.Errorf("fenced block lost: %q", got)
	}
	if !strings.Contains(got, "cp source target") {
		t.Errorf("command text lost: %q", got)
	}
	// No double newlines inside the code block (extra blank line bug)
	if strings.Contains(got, "```\n\n") {
		t.Errorf("double newline after opening fence: %q", got)
	}
	if strings.Contains(got, "\n\n```") {
		t.Errorf("double newline before closing fence: %q", got)
	}
}

func TestFormatMessage_MultilineBashBlock(t *testing.T) {
	in := "```bash\necho hello\ncd /tmp\nls -la\n```"
	got := formatMessage(in)
	if !strings.Contains(got, "echo hello") {
		t.Errorf("multiline block content lost: %q", got)
	}
	if !strings.Contains(got, "ls -la") {
		t.Errorf("multiline block last line lost: %q", got)
	}
}

func TestFormatMessage_BackslashInBashBlock(t *testing.T) {
	// Backslashes inside code blocks should be escaped as \\ per MDV2 spec
	in := "```bash\necho \"hello\\nworld\"\n```"
	got := formatMessage(in)
	// Should contain double-escaped backslash
	if !strings.Contains(got, `\\`) {
		t.Errorf("backslash not escaped in code block: %q", got)
	}
}

func TestFormatMessage_BacktickInBashBlock(t *testing.T) {
	// Backticks inside code blocks should be escaped
	in := "```bash\necho `date`\n```"
	got := formatMessage(in)
	if !strings.Contains(got, `\`+"`") {
		t.Errorf("backtick not escaped in code block: %q", got)
	}
}

func TestFormatMessage_InlineCode(t *testing.T) {
	in := "Use `ls -la` to list files."
	got := formatMessage(in)
	if !strings.Contains(got, "`ls -la`") {
		t.Errorf("inline code lost: %q", got)
	}
}

func TestFormatMessage_Bold(t *testing.T) {
	in := "This is **bold** text."
	got := formatMessage(in)
	// **bold** → *bold* in MDV2
	if strings.Contains(got, "**") {
		t.Errorf("double asterisks should be converted: %q", got)
	}
	if !strings.Contains(got, "*bold*") {
		t.Errorf("bold not converted: %q", got)
	}
}

func TestFormatMessage_Italic(t *testing.T) {
	in := "This is *italic* text."
	got := formatMessage(in)
	// *italic* → _italic_ in MDV2
	if !strings.Contains(got, "_italic_") {
		t.Errorf("italic not converted: %q", got)
	}
}

func TestFormatMessage_Strikethrough(t *testing.T) {
	in := "This is ~~deleted~~ text."
	got := formatMessage(in)
	if !strings.Contains(got, "~deleted~") {
		t.Errorf("strikethrough not converted: %q", got)
	}
	if strings.Contains(got, "~~") {
		t.Errorf("double tilde should be single: %q", got)
	}
}

func TestFormatMessage_Header(t *testing.T) {
	in := "## Title"
	got := formatMessage(in)
	// ## Title → *Title*
	if !strings.Contains(got, "*Title*") {
		t.Errorf("header not converted: %q", got)
	}
}

func TestFormatMessage_Link(t *testing.T) {
	in := "[click](https://example.com)"
	got := formatMessage(in)
	if !strings.Contains(got, "[") {
		t.Errorf("link lost: %q", got)
	}
	if !strings.Contains(got, "https://example.com") {
		t.Errorf("URL lost: %q", got)
	}
}

func TestFormatMessage_PurePlainText(t *testing.T) {
	in := "Hello world, no special formatting."
	got := formatMessage(in)
	if got == "" {
		t.Error("empty output for plain text")
	}
}

func TestFormatMessage_EmptyInput(t *testing.T) {
	if got := formatMessage(""); got != "" {
		t.Errorf("empty input should return empty: %q", got)
	}
}

func TestFormatMessage_LinkWithParentheses(t *testing.T) {
	in := "[wiki](https://en.wikipedia.org/wiki/Foo_(bar))"
	got := formatMessage(in)
	// MDV2 requires ) inside URL to be escaped → Foo_\(bar\)
	if !strings.Contains(got, "Foo_\\(bar\\)") {
		t.Errorf("link with parens not properly escaped for MDV2: %q", got)
	}
	if !strings.Contains(got, "[wiki]") {
		t.Errorf("link text lost: %q", got)
	}
}

func TestStripMdv2(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"escape backslashes", `\*\*bold\*\*`, "bold"},
		{"bold markers", "*bold*", "bold"},
		{"double bold", "**bold**", "bold"},
		{"italic markers", "_italic_", "italic"},
		{"strikethrough", "~strike~", "strike"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripMdv2(tt.in)
			if got != tt.want {
				t.Errorf("stripMdv2(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatMessage_ComplexBashWithSurroundingText(t *testing.T) {
	in := `Here is the command:

` + "```" + `bash
curl -fsSL https://example.com/install.sh | bash
` + "```" + `

This will install the package.`

	got := formatMessage(in)
	if !strings.Contains(got, "curl") {
		t.Errorf("curl command lost: %q", got)
	}
	if !strings.Contains(got, "install the package") {
		t.Errorf("surrounding text lost: %q", got)
	}
}

func TestFormatMessage_SingleLineShellBlock(t *testing.T) {
	// Single-line shell fenced block should be preserved
	in := "```shell\ncp source target\n```"
	got := formatMessage(in)
	if !strings.Contains(got, "cp source target") {
		t.Errorf("single-line shell block lost: %q", got)
	}
}

func TestFormatMessage_MultipleCodeBlocks(t *testing.T) {
	in := "First:\n`code1`\n\nSecond:\n" + "```\ncode2\n```" + "\n\nThird:\n`code3`"
	got := formatMessage(in)
	if !strings.Contains(got, "code1") {
		t.Errorf("first inline code lost: %q", got)
	}
	if !strings.Contains(got, "code2") {
		t.Errorf("fenced code lost: %q", got)
	}
	if !strings.Contains(got, "code3") {
		t.Errorf("second inline code lost: %q", got)
	}
}

func TestWrapMarkdownTables(t *testing.T) {
	in := "| Name | Value |\n|------|-------|\n| A | 1 |\n| B | 2 |"
	got := _wrapMarkdownTables(in)
	if !strings.Contains(got, "•") {
		t.Errorf("table not converted to bullets: %q", got)
	}
	if strings.Contains(got, "|--") {
		t.Errorf("separator row should be removed: %q", got)
	}
}

func TestWrapMarkdownTables_InCodeBlock(t *testing.T) {
	in := "```\n| Name | Value |\n|------|-------|\n```"
	got := _wrapMarkdownTables(in)
	// Table inside code block should NOT be converted
	if strings.Contains(got, "•") {
		t.Errorf("table inside code block should not be converted: %q", got)
	}
}

func TestFormatMessage_Spoiler(t *testing.T) {
	in := "||secret text||"
	got := formatMessage(in)
	if !strings.Contains(got, "||secret text||") {
		t.Errorf("spoiler not preserved: %q", got)
	}
}

func TestFormatMessage_BacktickLanguageTag(t *testing.T) {
	// Code block with language tag should preserve the tag
	in := "```python\nprint('hello')\n```"
	got := formatMessage(in)
	if !strings.Contains(got, "print") {
		t.Errorf("code content lost: %q", got)
	}
}
