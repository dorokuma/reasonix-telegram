// reasonix-telegram: markdown-to-Telegram-HTML converter.
//
// Converts standard markdown (model output) to Telegram HTML subset that
// our SOUL defines: <b>, <i>, <u>, <s>, <code>, <pre>, <a href="">,
// <blockquote>, <span class="tg-spoiler">.
//
// Strategy (mirrors Hermes TelegramAdapter.format_message):
// 1. Extract code blocks / inline code into placeholders (never touch content)
// 2. Convert markdown syntax to HTML tags in order
// 3. HTML-escape remaining text
// 4. Restore placeholders
package main

import (
	"regexp"
	"strings"
)

// markdown formatting regexps — compiled once.
var (
	// Fenced code block: ```lang? ... ```
	reFenced = regexp.MustCompile("```" + `(?:[^\n]*\n)?([\s\S]*?)` + "```")
	// Inline code: `...`
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	// Link: [text](url)
	reLink = regexp.MustCompile(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	// Header: # ... up to ######
	reHeader = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	// Bold: **text** or __text__
	reBold = regexp.MustCompile(`\*\*(.+?)\*\*`)
	// Italic: *text* (single asterisk, not across newlines)
	reItalic = regexp.MustCompile(`\*([^*\n]+)\*`)
	// Strikethrough: ~~text~~
	reStrike = regexp.MustCompile(`~~(.+?)~~`)
	// Spoiler: ||text||
	reSpoiler = regexp.MustCompile(`\|\|(.+?)\|\|`)
	// Unordered list item: - or * at line start
	reUlItem = regexp.MustCompile(`(?m)^\s*[-*+]\s+(.+)$`)
	// Ordered list item: 1. at line start
	reOlItem = regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+)$`)
	// Blockquote: > at line start
	reBlockquote = regexp.MustCompile(`(?m)^>\s?(.*)$`)
	// Horizontal rule: --- / *** / ___
	reHr = regexp.MustCompile(`(?m)^\s*[-*_]{3,}\s*$`)
)

// formatForTelegram converts standard markdown to Telegram-compatible HTML.
// Safe to call with any model output; non-markdown text is HTML-escaped.
func formatForTelegram(content string) string {
	if content == "" {
		return content
	}

	placeholders := map[string]string{}
	counter := 0
	ph := func(value string) string {
		key := "\x00PH\x00" + itoa(counter) + "\x00"
		counter++
		placeholders[key] = value
		return key
	}

	text := content

	// 0) Horizontal rules → full-width separator
	text = reHr.ReplaceAllString(text, "\n━━━━━━━━━━━━━━\n")

	// 1) Protect fenced code blocks
	text = reFenced.ReplaceAllStringFunc(text, func(match string) string {
		// Extract language if present
		inner := match
		if idx := strings.Index(inner, "\n"); idx >= 0 {
			_ = strings.TrimSpace(inner[3:idx]) // lang hint, unused for now
			inner = inner[idx+1:]
		} else {
			inner = inner[3:]
		}
		if strings.HasSuffix(inner, "```") {
			inner = inner[:len(inner)-3]
		}
		// Trim trailing newline from the code body
		inner = strings.TrimRight(inner, "\n")
		inner = htmlEscape(inner)
		return ph("<pre>" + inner + "</pre>")
	})

	// 2) Protect inline code
	text = reInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[1 : len(match)-1]
		return ph("<code>" + htmlEscape(inner) + "</code>")
	})

	// 3) Convert links [text](url) → <a href="url">text</a>
	text = reLink.ReplaceAllStringFunc(text, func(match string) string {
		parts := reLink.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		display := htmlEscape(parts[1])
		url := htmlEscape(parts[2])
		return ph("<a href=\"" + url + "\">" + display + "</a>")
	})

	// 4) Convert headers → <b>header</b> (strip inner markdown markers)
	text = reHeader.ReplaceAllStringFunc(text, func(match string) string {
		parts := reHeader.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := strings.TrimSpace(parts[1])
		// Strip bold/italic markers from header text
		inner = strings.ReplaceAll(inner, "**", "")
		inner = strings.ReplaceAll(inner, "__", "")
		inner = strings.ReplaceAll(inner, "*", "")
		return ph("<b>" + htmlEscape(inner) + "</b>")
	})

	// 5) Convert bold **text**
	text = reBold.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBold.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<b>" + htmlEscape(parts[1]) + "</b>")
	})

	// 6) Convert italic *text* (single asterisk)
	text = reItalic.ReplaceAllStringFunc(text, func(match string) string {
		parts := reItalic.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<i>" + htmlEscape(parts[1]) + "</i>")
	})

	// 7) Convert strikethrough ~~text~~
	text = reStrike.ReplaceAllStringFunc(text, func(match string) string {
		parts := reStrike.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<s>" + htmlEscape(parts[1]) + "</s>")
	})

	// 8) Convert spoiler ||text||
	text = reSpoiler.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSpoiler.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph(`<span class="tg-spoiler">` + htmlEscape(parts[1]) + `</span>`)
	})

	// 9) Convert unordered list items
	text = reUlItem.ReplaceAllStringFunc(text, func(match string) string {
		parts := reUlItem.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("• " + htmlEscape(strings.TrimSpace(parts[1])))
	})

	// 10) Convert ordered list items
	text = reOlItem.ReplaceAllStringFunc(text, func(match string) string {
		parts := reOlItem.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph(htmlEscape(strings.TrimSpace(parts[1])))
	})

	// 11) Convert blockquotes
	text = reBlockquote.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBlockquote.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := htmlEscape(strings.TrimSpace(parts[1]))
		return ph("<blockquote>" + inner + "</blockquote>")
	})

	// 12) HTML-escape remaining plain text (&, <, >)
	text = htmlEscape(text)

	// 13) Restore placeholders (reverse order so nested resolve correctly)
	keys := make([]string, 0, len(placeholders))
	for k := range placeholders {
		keys = append(keys, k)
	}
	// Reverse for safe nesting
	for i := len(keys) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, keys[i], placeholders[keys[i]])
	}

	return text
}

// htmlEscape escapes &, <, > for safe HTML insertion.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// itoa is a small int-to-string (avoids fmt import just for this).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}
