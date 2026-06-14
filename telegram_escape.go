package main

import "regexp"

// _MDV2_ESCAPE_RE matches every character that MarkdownV2 requires to be
// backslash-escaped when it appears outside a code span or fenced code block.
var _MDV2_ESCAPE_RE = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)

// _escapeMdv2 escapes MarkdownV2 special characters with backslash.
// Only needed for messages still using ParseMode=MarkdownV2 (clarify/approval).
func _escapeMdv2(text string) string {
	return _MDV2_ESCAPE_RE.ReplaceAllString(text, `\$1`)
}
