package main

import (
	"fmt"
	"regexp"
	"strings"
)

// ── Telegram MarkdownV2 formatting engine ─────────────────────────────────
//
// Ported from hermes-agent gateway/platforms/telegram.py format_message().
// Converts standard Markdown (as emitted by LLMs) into Telegram MarkdownV2
// syntax. Protected regions (code blocks, inline code) are extracted first
// via placeholders so their contents are never modified.

// fencedRe matches fenced code blocks: ```optional-lang\n...\n```
var fencedRe = regexp.MustCompile("(?s)`{3,}[^\\n]*\\n.*?`{3,}")

// inlineCodeRe matches inline code spans: `...`
var inlineCodeRe = regexp.MustCompile("`[^`]+`")

// linkRe matches markdown links: [text](url)
var linkRe = regexp.MustCompile(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)

// headerRe matches markdown headers: ## Title
var headerRe = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)

// boldRe matches markdown bold: **text**
var boldRe = regexp.MustCompile(`\*\*(.+?)\*\*`)

// italicRe matches markdown italic: *text* (single asterisk, not across newlines)
var italicRe = regexp.MustCompile(`\*([^*]+)\*`)

// strikeRe matches markdown strikethrough: ~~text~~
var strikeRe = regexp.MustCompile(`~~(.+?)~~`)

// spoilerRe matches markdown spoiler: ||text||
var spoilerRe = regexp.MustCompile(`\|\|(.+?)\|\|`)

// blockquoteRe matches markdown blockquotes: > text, >> text, etc.
var blockquoteRe = regexp.MustCompile(`(?m)^((?:\*\*)?>{1,3}) (.+)$`)

// tableSeparatorRe matches GFM table separator rows: |---|---|
var tableSeparatorRe = regexp.MustCompile(`^\s*\|?(\s*:?-{2,}:?\s*\|)+\s*:?-{2,}:?\s*\|?\s*$`)

// bareCharsRe matches unescaped (){} for safety-net escaping.
var bareCharsRe = regexp.MustCompile(`[(){}]`)

// stripMdv2 pre-compiled regexes
var _STRIP_ESCAPE_RE = regexp.MustCompile(`\\([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)
var _STRIP_BOLD_RE = regexp.MustCompile(`\*\*([^*]+)\*\*`)
var _STRIP_ITALIC_RE = regexp.MustCompile(`\*([^*]+)\*`)
var _STRIP_UNDER_RE = regexp.MustCompile(`_([^_\n]+)_`)
var _STRIP_STRIKE_RE = regexp.MustCompile(`~([^~]+)~`)

// placeholder token prefix/suffix
const _phPrefix = "\x00PH"
const _phSuffix = "\x00"

// ── Table helpers ────────────────────────────────────────────────────────

func isTableRow(line string) bool {
	stripped := strings.TrimSpace(line)
	return stripped != "" && strings.Contains(stripped, "|")
}

func _splitMarkdownTableRow(line string) []string {
	stripped := strings.TrimSpace(line)
	stripped = strings.TrimPrefix(stripped, "|")
	stripped = strings.TrimSuffix(stripped, "|")
	parts := strings.Split(stripped, "|")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

func renderTableBlockForTelegram(block []string) string {
	if len(block) < 3 {
		return strings.Join(block, "\n")
	}
	headers := _splitMarkdownTableRow(block[0])
	if len(headers) < 2 {
		return strings.Join(block, "\n")
	}

	// Detect row-label column: data rows have one more cell than headers
	hasRowLabelCol := false
	if len(block) > 2 {
		firstData := _splitMarkdownTableRow(block[2])
		if len(firstData) == len(headers)+1 {
			hasRowLabelCol = true
		}
	}

	var rendered []string
	for idx, row := range block[2:] {
		cells := _splitMarkdownTableRow(row)
		var heading string
		var dataCells []string

		if hasRowLabelCol {
			if len(cells) > 0 && cells[0] != "" {
				heading = cells[0]
			} else {
				heading = fmt.Sprintf("Row %d", idx+1)
			}
			dataCells = cells[1:]
		} else {
			heading = ""
			for _, c := range cells {
				if c != "" {
					heading = c
					break
				}
			}
			if heading == "" {
				heading = fmt.Sprintf("Row %d", idx+1)
			}
			dataCells = cells
		}

		// Pad or trim dataCells to match headers
		if len(dataCells) < len(headers) {
			for len(dataCells) < len(headers) {
				dataCells = append(dataCells, "")
			}
		} else if len(dataCells) > len(headers) {
			dataCells = dataCells[:len(headers)]
		}

		// Build bullets
		var bullets []string
		for i, header := range headers {
			value := ""
			if i < len(dataCells) {
				value = dataCells[i]
			}
			if !hasRowLabelCol && value == heading {
				continue
			}
			bullets = append(bullets, fmt.Sprintf("• %s: %s", header, value))
		}

		group := fmt.Sprintf("*%s*\n%s", heading, strings.Join(bullets, "\n"))
		rendered = append(rendered, group)
	}
	return strings.Join(rendered, "\n\n")
}

// wrapMarkdownTables rewrites GFM pipe tables into Telegram-friendly bullet
// groups. Tables inside fenced code blocks are left alone.
func wrapMarkdownTables(text string) string {
	if !strings.Contains(text, "|") || !strings.Contains(text, "-") {
		return text
	}

	lines := strings.Split(text, "\n")
	var out []string
	inFence := false
	i := 0
	for i < len(lines) {
		line := lines[i]
		stripped := strings.TrimLeft(line, " \t")

		// Track fenced code blocks
		if strings.HasPrefix(stripped, "```") {
			inFence = !inFence
			out = append(out, line)
			i++
			continue
		}
		if inFence {
			out = append(out, line)
			i++
			continue
		}

		// Look for a header row (contains '|') immediately followed by a separator row
		if strings.Contains(line, "|") && i+1 < len(lines) && tableSeparatorRe.MatchString(lines[i+1]) {
			tableBlock := []string{line, lines[i+1]}
			j := i + 2
			for j < len(lines) && isTableRow(lines[j]) {
				tableBlock = append(tableBlock, lines[j])
				j++
			}
			out = append(out, renderTableBlockForTelegram(tableBlock))
			i = j
			continue
		}

		out = append(out, line)
		i++
	}
	return strings.Join(out, "\n")
}

// ── Core format_message ──────────────────────────────────────────────────

// formatMessage converts standard Markdown (as emitted by LLMs) into Telegram
// MarkdownV2 format. Protected regions (code blocks, inline code) are extracted
// first via placeholders so their contents are never modified. Standard markdown
// constructs (headers, bold, italic, links) are translated to MarkdownV2 syntax,
// and all remaining special characters are escaped.
func formatMessage(content string) string {
	if content == "" {
		return content
	}

	// Placeholder system: stash values behind tokens that survive escaping
	placeholders := make(map[string]string)
	counter := 0
	ph := func(value string) string {
		key := fmt.Sprintf("%s%d%s", _phPrefix, counter, _phSuffix)
		counter++
		placeholders[key] = value
		return key
	}

	text := content

	// 0) Rewrite GFM-style pipe tables into Telegram-friendly row groups
	text = wrapMarkdownTables(text)

	// 1) Protect fenced code blocks (``` ... ```)
	// Per MarkdownV2 spec, \ and ` inside pre/code must be escaped.
	text = fencedRe.ReplaceAllStringFunc(text, func(m string) string {
		raw := m
		// Split off opening ``` (with optional language) and closing ```
		openEnd := strings.Index(raw[3:], "\n")
		if openEnd < 0 {
			// No newline after opening — bare ```
			return ph(raw)
		}
		openEnd += 4 // +3 for offset into raw, +1 to include the \n
		opening := raw[:openEnd]
		bodyAndClose := raw[openEnd:]
		body := bodyAndClose[:len(bodyAndClose)-3] // remove closing ```

		// Escape \ and ` inside code blocks per MDV2 spec
		body = strings.ReplaceAll(body, `\`, `\\`)
		body = strings.ReplaceAll(body, "`", "\\`")
		return ph(opening + body + "```")
	})

	// 2) Protect inline code (`...`)
	// Escape \ inside inline code per MDV2 spec.
	text = inlineCodeRe.ReplaceAllStringFunc(text, func(m string) string {
		return ph(strings.ReplaceAll(m, `\`, `\\`))
	})

	// 3) Convert markdown links [text](url)
	// Escape the display text; inside the URL only ')' and '\' need escaping per MDV2.
	text = linkRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := linkRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		display := escapeMdv2(parts[1])
		url := strings.ReplaceAll(parts[2], `\`, `\\`)
		url = strings.ReplaceAll(url, `)`, `\)`)
		return ph(fmt.Sprintf("[%s](%s)", display, url))
	})

	// 4) Convert markdown headers (## Title) → bold *Title*
	text = headerRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := headerRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		inner := strings.TrimSpace(parts[1])
		// Strip redundant bold markers inside a header
		inner = boldRe.ReplaceAllString(inner, `$1`)
		return ph(fmt.Sprintf("*%s*", escapeMdv2(inner)))
	})

	// 5) Convert bold: **text** → *text* (MarkdownV2 bold)
	text = boldRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := boldRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return ph(fmt.Sprintf("*%s*", escapeMdv2(parts[1])))
	})

	// 6) Convert italic: *text* → _text_ (MarkdownV2 italic)
	// [^*\n]+ prevents matching across newlines (protects bullet lists)
	text = italicRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := italicRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return ph(fmt.Sprintf("_%s_", escapeMdv2(parts[1])))
	})

	// 7) Convert strikethrough: ~~text~~ → ~text~ (MarkdownV2)
	text = strikeRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := strikeRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return ph(fmt.Sprintf("~%s~", escapeMdv2(parts[1])))
	})

	// 8) Convert spoiler: ||text|| → ||text|| (protect from | escaping)
	text = spoilerRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := spoilerRe.FindStringSubmatch(m)
		if len(parts) < 2 {
			return m
		}
		return ph(fmt.Sprintf("||%s||", escapeMdv2(parts[1])))
	})

	// 9) Convert blockquotes: > at line start → protect > from escaping
	// Handles regular blockquotes (>) and expandable blockquotes (**>)
	text = blockquoteRe.ReplaceAllStringFunc(text, func(m string) string {
		parts := blockquoteRe.FindStringSubmatch(m)
		if len(parts) < 3 {
			return m
		}
		prefix := parts[1]
		content := parts[2]
		// Expandable blockquote: ** prefix with trailing ||
		if strings.HasPrefix(prefix, "**") && strings.HasSuffix(content, "||") {
			return ph(fmt.Sprintf("%s %s||", prefix, escapeMdv2(content[:len(content)-2])))
		}
		return ph(fmt.Sprintf("%s %s", prefix, escapeMdv2(content)))
	})

	// 10) Escape remaining special characters in plain text
	text = escapeMdv2(text)

	// 11) Restore placeholders in reverse insertion order so that
	// nested references resolve correctly.
	keys := make([]string, 0, len(placeholders))
	for k := range placeholders {
		keys = append(keys, k)
	}
	for i := len(keys) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, keys[i], placeholders[keys[i]])
	}

	// 12) Safety net: escape unescaped (){} that slipped through placeholder
	// processing. Split into code/non-code segments so we never touch content
	// inside ``` or ` spans.
	codeSplit := _splitByCode(text)
	var safeParts []string
	for idx, seg := range codeSplit {
		if idx%2 == 1 {
			// Inside code span/block — leave untouched
			safeParts = append(safeParts, seg)
		} else {
			// Outside code — escape bare (){}
			safeParts = append(safeParts, escapeBareChars(seg))
		}
	}
	text = strings.Join(safeParts, "")
	return text
}

// _splitByCode splits text into alternating segments: [outside-code, inside-code, outside-code, ...]
func _splitByCode(text string) []string {
	// Pattern matches fenced blocks and inline code
	re := regexp.MustCompile("(?s)`{3,}[^\n]*\n.*?`{3,}|`[^`]+`")
	var result []string
	last := 0
	for _, loc := range re.FindAllStringIndex(text, -1) {
		if loc[0] > last {
			result = append(result, text[last:loc[0]])
		}
		result = append(result, text[loc[0]:loc[1]])
		last = loc[1]
	}
	if last < len(text) {
		result = append(result, text[last:])
	}
	return result
}

// escapeBareChars escapes bare (){} outside code blocks.
func escapeBareChars(seg string) string {
	var out strings.Builder
	runes := []rune(seg)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		if ch != '(' && ch != ')' && ch != '{' && ch != '}' {
			out.WriteRune(ch)
			continue
		}
		// Already escaped
		if i > 0 && runes[i-1] == '\\' {
			out.WriteRune(ch)
			continue
		}
		// ( that opens a MarkdownV2 link [text](url)
		if ch == '(' && i > 0 && runes[i-1] == ']' {
			out.WriteRune(ch)
			continue
		}
		// ) that closes a link URL
		if ch == ')' {
			before := string(runes[:i])
			if strings.Contains(before, "](http") || strings.Contains(before, "](") {
				// Check depth
				depth := 0
				searchLimit := i - 2000
				if searchLimit < 0 {
					searchLimit = 0
				}
				found := false
				for j := i - 1; j >= searchLimit; j-- {
					if runes[j] == '(' {
						depth--
					}
					if depth < 0 {
						if j > 0 && runes[j-1] == ']' {
							found = true
						}
						break
					}
					if runes[j] == ')' {
						depth++
					}
				}
				if found {
					out.WriteRune(ch)
					continue
				}
			}
		}
		out.WriteString("\\")
		out.WriteRune(ch)
	}
	return out.String()
}

// ── _stripMdv2 ───────────────────────────────────────────────────────────

// stripMdv2 strips MarkdownV2 escape backslashes and formatting markers to
// produce clean plain text. Used as fallback when MarkdownV2 parsing fails.
func stripMdv2(text string) string {
	if text == "" {
		return text
	}
	// Remove escape backslashes before special characters: \* → *, \( → (, etc.
	cleaned := _STRIP_ESCAPE_RE.ReplaceAllString(text, `$1`)
	// Remove standard markdown bold (**text** → text) BEFORE MarkdownV2 bold
	cleaned = _STRIP_BOLD_RE.ReplaceAllString(cleaned, `$1`)
	// Remove MarkdownV2 bold markers that formatMessage converted from **bold**
	cleaned = _STRIP_ITALIC_RE.ReplaceAllString(cleaned, `$1`)
	// Remove MarkdownV2 italic markers: _text_ → text
	// Go regex no lookahead — simple match _..._
	cleaned = _STRIP_UNDER_RE.ReplaceAllString(cleaned, `$1`)
	// Remove strikethrough: ~text~ → text (not ~~)
	cleaned = _STRIP_STRIKE_RE.ReplaceAllString(cleaned, `$1`)
	return cleaned
}
