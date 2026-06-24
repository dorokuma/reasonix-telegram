package main

import "regexp"

// mdv2EscapeRe matches every character that MarkdownV2 requires to be
// backslash-escaped when it appears outside a code span or fenced code block.
// Kept for backward compatibility with clarify/approval paths that call
// escapeMdv2() directly.
var mdv2EscapeRe = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)

// escapeMdv2 escapes MarkdownV2 special characters with backslash.
// For messages that still use ParseMode=MarkdownV2 directly (clarify/approval).
// For the full Markdown→MarkdownV2 conversion, use formatMessage() instead.
func escapeMdv2(text string) string {
	return mdv2EscapeRe.ReplaceAllString(text, `\$1`)
}
