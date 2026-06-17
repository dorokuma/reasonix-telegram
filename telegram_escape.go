package main

import "regexp"

// _MDV2_ESCAPE_RE matches every character that MarkdownV2 requires to be
// backslash-escaped when it appears outside a code span or fenced code block.
// Kept for backward compatibility with clarify/approval paths that call
// _escapeMdv2() directly.
var _MDV2_ESCAPE_RE = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)

// _escapeMdv2 escapes MarkdownV2 special characters with backslash.
// For messages that still use ParseMode=MarkdownV2 directly (clarify/approval).
// For the full Markdown→MarkdownV2 conversion, use formatMessage() instead.
func _escapeMdv2(text string) string {
	return _MDV2_ESCAPE_RE.ReplaceAllString(text, `\$1`)
}
