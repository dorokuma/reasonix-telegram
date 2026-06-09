// reasonix-telegram: markdown-to-Telegram-MarkdownV2 converter.
// Ported from Hermes gateway/platforms/telegram.py
// (TelegramAdapter.format_message, _escape_mdv2, _strip_mdv2,
//  _wrap_markdown_tables, _render_table_block_for_telegram).
//
// Strategy (matches Hermes exactly):
//   0. Rewrite GFM pipe tables into bullet groups via _wrapMarkdownTables
//   1. Extract fenced code blocks → placeholder
//   2. Extract inline code → placeholder
//   3. Convert links [text](url) → placeholder
//   4. Convert headers # Title → placeholder
//   5. Convert bold **text** → placeholder
//   6. Convert italic *text* → placeholder
//   7. Convert strikethrough ~~text~~ → placeholder
//   8. Convert spoiler ||text|| → placeholder
//   9. Convert blockquote > text → placeholder
//  10. Escape all remaining MDV2 special chars
//  11. Restore placeholders in reverse insertion order
//  12. Safety net: escape bare ( ) { } outside code segments
//
// Reasonix-specific additions kept:
//   - stripHookMessages (RTK hook block text filtering)
//   - stripLatexDelimiters (\( \) \[ \] → plain)
//   - toolEmoji / formatToolArgs / formatToolResult /
//     formatDirListing / formatSearchResult (tool output decoration)
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// _MDV2_ESCAPE_RE matches every character that MarkdownV2 requires to be
// backslash-escaped when it appears outside a code span or fenced code block.
var _MDV2_ESCAPE_RE = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#\+\-=|{}.!\\])`)

func _escapeMdv2(text string) string {
	return _MDV2_ESCAPE_RE.ReplaceAllString(text, `\$1`)
}

// _stripMdv2 removes MarkdownV2 escape backslashes and formatting markers so
// the plain-text fallback (used when entity parsing fails) does not show
// stray syntax characters.
func _stripMdv2(text string) string {
	// Remove escape backslashes before special characters
	cleaned := regexp.MustCompile(`\\([_*\[\]()~`+"`"+`>#\+\-=|{}.!\\])`).ReplaceAllString(text, `$1`)
	// Remove MarkdownV2 bold markers
	cleaned = regexp.MustCompile(`\*([^*]+)\*`).ReplaceAllString(cleaned, `$1`)
	// Remove MarkdownV2 italic markers. Python's r'(?<!\w)_([^_]+)_(?!\w)'
	// cannot be expressed in Go's RE2 (no lookarounds), so we walk runes
	// manually and require non-word chars at both ends. This preserves
	// snake_case identifiers like my_variable_name.
	cleaned = stripItalicUnderscores(cleaned)
	// Remove MarkdownV2 strikethrough markers
	cleaned = regexp.MustCompile(`~([^~]+)~`).ReplaceAllString(cleaned, `$1`)
	// Remove MarkdownV2 spoiler markers
	cleaned = regexp.MustCompile(`\|\|([^|]+)\|\|`).ReplaceAllString(cleaned, `$1`)
	return cleaned
}

// stripItalicUnderscores strips paired _X_ markers where neither side is
// adjacent to a word char. Equivalent to Python's (?<!\w) ... (?!\w) for
// this restricted scope.
func stripItalicUnderscores(s string) string {
	runes := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i < len(runes) {
		if runes[i] == '_' {
			// Find the next _ after at least one non-_ rune.
			j := i + 1
			for j < len(runes) && runes[j] != '_' {
				j++
			}
			if j > i+1 && j < len(runes) {
				// Require non-word char before opening _.
				beforeOK := i == 0 || !isWordRune(runes[i-1])
				// Require non-word char after closing _.
				afterOK := j == len(runes)-1 || !isWordRune(runes[j+1])
				if beforeOK && afterOK {
					b.WriteString(string(runes[i+1 : j]))
					i = j + 1
					continue
				}
			}
		}
		b.WriteRune(runes[i])
		i++
	}
	return b.String()
}

func isWordRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9') || r == '_'
}

// regex compiled once.
var (
	reFenced    = regexp.MustCompile("```" + `(?:[^\n]*\n)?([\s\S]*?)` + "```")
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reLink      = regexp.MustCompile(`\[([^\]]+)\]\(([^()]*(?:\([^()]*\)[^()]*)*)\)`)
	reHeader    = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`)
	reBold      = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic    = regexp.MustCompile(`\*([^*\n]+)\*`)
	reStrike    = regexp.MustCompile(`~~(.+?)~~`)
	reSpoiler   = regexp.MustCompile(`\|\|(.+?)\|\|`)
	reBlockquote = regexp.MustCompile(`(?m)^((?:\*\*)?>{1,3})\s+(.+)$`)
	reHr        = regexp.MustCompile(`(?m)^\s*[-*_]{3,}\s*$`)

	// GFM table detection
	reTableSep = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

	// LaTeX delimiter stripping
	reLatexDisplay = regexp.MustCompile(`\\\[([\s\S]*?)\\\]`)
	reLatexInline  = regexp.MustCompile(`\\\(([\s\S]*?)\\\)`)
)

// _TABLE_SEPARATOR_RE_COMMENT: kept as a docstring for parity with Hermes.
// Matches a GFM table delimiter row: optional outer pipes, cells containing
// only dashes (with optional leading/trailing colons for alignment)
// separated by '|'.  Requires at least one internal '|' so lone '---'
// horizontal rules are NOT matched.

// wrapMarkdownTables rewrites GFM pipe tables into Telegram-friendly
// bold-heading + bullet groups. Ported from Hermes
// _wrap_markdown_tables + _render_table_block_for_telegram.
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

		// Track fenced code blocks — never touch content inside.
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

		// Header row: contains '|' AND next line is a separator.
		if !strings.Contains(line, "|") || i+1 >= len(lines) || !reTableSep.MatchString(lines[i+1]) {
			out = append(out, line)
			i++
			continue
		}

		// Consume the table block: header, separator, then data rows.
		tableBlock := []string{line, lines[i+1]}
		j := i + 2
		for j < len(lines) {
			row := strings.TrimSpace(lines[j])
			if row == "" || !strings.Contains(lines[j], "|") {
				break
			}
			tableBlock = append(tableBlock, lines[j])
			j++
		}
		out = append(out, renderTableBlockForTelegram(tableBlock))
		i = j
	}
	return strings.Join(out, "\n")
}

func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "|") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "|") {
		s = s[:len(s)-1]
	}
	cells := strings.Split(s, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func renderTableBlockForTelegram(tableBlock []string) string {
	if len(tableBlock) < 3 {
		return strings.Join(tableBlock, "\n")
	}

	headers := splitTableRow(tableBlock[0])
	if len(headers) < 2 {
		return strings.Join(tableBlock, "\n")
	}

	// Detect row-label column: present when data rows have one more cell
	// than the header row.
	var firstDataRow []string
	if len(tableBlock) > 2 {
		firstDataRow = splitTableRow(tableBlock[2])
	}
	hasRowLabelCol := len(firstDataRow) == len(headers)+1

	var groups []string
	for index, row := range tableBlock[2:] {
		cells := splitTableRow(row)
		var heading string
		var dataCells []string

		if hasRowLabelCol {
			heading = cells[0]
			if heading == "" {
				heading = fmt.Sprintf("Row %d", index)
			}
			if len(cells) > 1 {
				dataCells = cells[1:]
			}
		} else {
			for _, c := range cells {
				if c != "" {
					heading = c
					break
				}
			}
			if heading == "" {
				heading = fmt.Sprintf("Row %d", index)
			}
			dataCells = cells
		}

		// Pad or trim to match headers length.
		for len(dataCells) < len(headers) {
			dataCells = append(dataCells, "")
		}
		if len(dataCells) > len(headers) {
			dataCells = dataCells[:len(headers)]
		}

		// Skip bullets that duplicate the heading (when no row-label col,
		// the first cell IS the heading).
		var bullets []string
		for i, h := range headers {
			if i < len(dataCells) {
				v := dataCells[i]
				if !hasRowLabelCol && v == heading {
					continue
				}
				bullets = append(bullets, "• "+h+": "+v)
			}
		}

		// Strip any existing bold/code markers from the cell content before
		// we wrap it as a bold heading. The cell may contain **bold** or
		// `code` markdown; double-wrapping would produce ****text**** or
		// **`**bold**`** which breaks the bold regex later.
		clean := strings.TrimSpace(heading)
		// Strip surrounding backticks (inline code) so that `**bold**`
		// becomes **bold** then bold, not **`**bold**`** which leaks.
		for strings.HasPrefix(clean, "`") {
			clean = strings.TrimPrefix(clean, "`")
		}
		for strings.HasSuffix(clean, "`") {
			clean = strings.TrimSuffix(clean, "`")
		}
		for strings.HasPrefix(clean, "**") {
			clean = strings.TrimPrefix(clean, "**")
		}
		for strings.HasSuffix(clean, "**") {
			clean = strings.TrimSuffix(clean, "**")
		}
		clean = strings.TrimSpace(clean)
		groupLines := []string{"**" + clean + "**"}
		groupLines = append(groupLines, bullets...)
		groups = append(groups, strings.Join(groupLines, "\n"))
	}

	return strings.Join(groups, "\n\n")
}

// formatForTelegram converts standard markdown to Telegram MarkdownV2.
// Safe to call with any model output; non-markdown text is MDV2-escaped.
// Returns the input unchanged for empty / nil-ish content.
//
// Ported verbatim from Hermes TelegramAdapter.format_message. The output
// is suitable for use with Telegram Bot API ParseMode="MarkdownV2".
func formatForTelegram(content string) string {
	if content == "" {
		return content
	}
	content = stripHookMessages(content)
	content = stripBackgroundJobs(content)
	if content == "" {
		return ""
	}

	// Hermes-style placeholder: \x00PH{counter}\x00 — null bytes survive
	// Telegram's text processing. Ordered slice + map: insertion order
	// tracked so restoration runs in reverse order (nested placeholders
	// resolve correctly).
	placeholders := map[string]string{}
	var phOrder []string
	counter := 0
	ph := func(value string) string {
		key := fmt.Sprintf("\x00PH%d\x00", counter)
		counter++
		placeholders[key] = value
		phOrder = append(phOrder, key)
		return key
	}

	text := content

	// 0a) Strip LaTeX delimiters \( \) \[ \] (Reasonix-specific).
	//     Run before the MDV2 pipeline so raw \( \) doesn't get escaped
	//     into something Telegram can't render.
	text = stripLatexDelimiters(text)

	// 0b) Rewrite GFM-style pipe tables into Telegram-friendly row groups
	//     before the normal MarkdownV2 conversions run.
	text = wrapMarkdownTables(text)

	// 1) Protect fenced code blocks (``` ... ```).
	//    Per MarkdownV2 spec, \ and ` inside pre/code must be escaped.
	text = reFenced.ReplaceAllStringFunc(text, func(match string) string {
		raw := match
		openEnd := 3
		if nl := strings.Index(raw[3:], "\n"); nl >= 0 {
			openEnd = 3 + nl + 1
		}
		opening := raw[:openEnd]
		bodyAndClose := raw[openEnd:]
		body := bodyAndClose[:len(bodyAndClose)-3]
		body = strings.ReplaceAll(body, "\\", "\\\\")
		body = strings.ReplaceAll(body, "`", "\\`")
		return ph(opening + body + "```")
	})

	// 2) Protect inline code (`...`).
	//    Escape \ inside inline code per MarkdownV2 spec.
	text = reInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		return ph(strings.ReplaceAll(match, "\\", "\\\\"))
	})

	// 3) Convert markdown links — escape display text; inside the URL
	//    only ')' and '\' need escaping per the MarkdownV2 spec.
	text = reLink.ReplaceAllStringFunc(text, func(match string) string {
		parts := reLink.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		display := _escapeMdv2(parts[1])
		url := strings.ReplaceAll(parts[2], "\\", "\\\\")
		url = strings.ReplaceAll(url, ")", "\\)")
		return ph("[" + display + "](" + url + ")")
	})

	// 4) Convert markdown headers (## Title) → bold *Title*.
	text = reHeader.ReplaceAllStringFunc(text, func(match string) string {
		parts := reHeader.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := strings.TrimSpace(parts[1])
		// Strip redundant ** that may appear inside a header
		inner = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(inner, `$1`)
		return ph("*" + _escapeMdv2(inner) + "*")
	})

	// 5) Convert bold: **text** → *text* (MarkdownV2 bold)
	text = reBold.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBold.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("*" + _escapeMdv2(parts[1]) + "*")
	})

	// 6) Convert italic: *text* → _text_ (MarkdownV2 italic)
	text = reItalic.ReplaceAllStringFunc(text, func(match string) string {
		parts := reItalic.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("_" + _escapeMdv2(parts[1]) + "_")
	})

	// 7) Convert strikethrough: ~~text~~ → ~text~ (MarkdownV2)
	text = reStrike.ReplaceAllStringFunc(text, func(match string) string {
		parts := reStrike.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("~" + _escapeMdv2(parts[1]) + "~")
	})

	// 8) Convert spoiler: ||text|| → ||text|| (protect from | escaping)
	text = reSpoiler.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSpoiler.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("||" + _escapeMdv2(parts[1]) + "||")
	})

	// 9) Convert blockquotes: > at line start → protect > from escaping.
	//    Handle both regular (>) and expandable (**> / **||) variants.
	text = reBlockquote.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBlockquote.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		prefix := parts[1]
		content := parts[2]
		// Expandable blockquote: prefix starts with ** and content ends with ||
		if strings.HasPrefix(prefix, "**") && strings.HasSuffix(content, "||") {
			return ph(prefix + " " + _escapeMdv2(content[:len(content)-2]) + "||")
		}
		return ph(prefix + " " + _escapeMdv2(content))
	})

	// 10) Escape remaining special characters in plain text.
	text = _escapeMdv2(text)

	// 11) Restore placeholders in reverse insertion order.
	for i := len(phOrder) - 1; i >= 0; i-- {
		key := phOrder[i]
		text = strings.ReplaceAll(text, key, placeholders[key])
	}

	// 12) Safety net: escape unescaped ( ) { } that slipped through
	//     placeholder processing. Split text into code/non-code segments
	//     so we never touch content inside ``` or ` spans.
	text = _safeEscapeBare(text)

	return text
}

// _safeEscapeBare escapes bare ( ) { } outside code blocks / inline code
// spans, preserving any that are part of MarkdownV2 link syntax
// ([text](url)). Ported from Hermes format_message step 12.
func _safeEscapeBare(text string) string {
	// Split into segments: even indices are plain text, odd indices are
	// code spans/blocks. We only touch the even (plain) segments.
	codeSplit := regexp.MustCompile("(?s)(```[\\s\\S]*?```|`[^`]+`)").Split(text, -1)
	// We need to keep the code spans themselves, so use FindAllStringIndex
	// and walk both lists in lockstep.
	codeSpans := regexp.MustCompile("(?s)(```[\\s\\S]*?```|`[^`]+`)").FindAllStringIndex(text, -1)

	var b strings.Builder
	plainIdx := 0
	codeIdx := 0
	cursor := 0
	// Re-walk by alternating between plain segments and code spans
	for plainIdx < len(codeSplit) {
		// Append this plain segment
		seg := codeSplit[plainIdx]
		// Process seg to escape bare ( ) { }
		processed := _escapeBareInSegment(seg)
		b.WriteString(processed)
		cursor += len(seg)

		// Append the next code span if any
		if codeIdx < len(codeSpans) {
			span := text[codeSpans[codeIdx][0]:codeSpans[codeIdx][1]]
			b.WriteString(span)
			cursor = codeSpans[codeIdx][1]
			codeIdx++
		}
		plainIdx++
	}
	return b.String()
}

// _escapeBareInSegment escapes bare ( ) { } in a plain (non-code) text
// segment, preserving those that are part of MarkdownV2 link syntax.
func _escapeBareInSegment(seg string) string {
	var b strings.Builder
	runes := []rune(seg)
	for i, r := range runes {
		if r != '(' && r != ')' && r != '{' && r != '}' {
			b.WriteRune(r)
			continue
		}
		// Already escaped?
		if i > 0 && runes[i-1] == '\\' {
			b.WriteRune(r)
			continue
		}
		// ( that opens a MarkdownV2 link [text](url)
		if r == '(' && i > 0 && runes[i-1] == ']' {
			b.WriteRune(r)
			continue
		}
		// ) that closes a link URL
		if r == ')' {
			// Look back: find the most recent unmatched '('. If it was
			// preceded by ']' then it's a link, leave alone.
			depth := 0
			keep := false
			for j := i - 1; j >= 0; j-- {
				if runes[j] == '(' {
					depth--
					if depth < 0 {
						if j > 0 && runes[j-1] == ']' {
							keep = true
						}
						break
					}
				} else if runes[j] == ')' {
					depth++
				}
			}
			if keep {
				b.WriteRune(r)
				continue
			}
		}
		b.WriteByte('\\')
		b.WriteRune(r)
	}
	return b.String()
}

// stripLatexDelimiters removes LaTeX math delimiters, keeping the
// expression content. (Reasonix-specific, not in Hermes.)
func stripLatexDelimiters(s string) string {
	s = reLatexDisplay.ReplaceAllString(s, "$1")
	s = reLatexInline.ReplaceAllString(s, "$1")
	return s
}

// toolEmoji returns an emoji for a Reasonix tool name.
// Mapped from Hermes' tool registry emoji assignments.
func toolEmoji(name string) string {
	switch name {
	case "bash":
		return "💻"
	case "python", "python3", "execute_code", "code":
		return "🐍"
	case "read_file", "cat":
		return "📖"
	case "write_file", "edit_file", "multi_edit":
		return "✍️"
	case "grep", "search_files", "codegraph_search":
		return "🔎"
	case "glob", "ls", "codegraph_files":
		return "📁"
	case "codegraph_callees", "codegraph_callers", "codegraph_context",
		"codegraph_explore", "codegraph_trace", "codegraph_impact":
		return "🔍"
	case "ask":
		return "❓"
	case "search_web", "web_search",
		"mcp__jina__search_web", "mcp__jina__search",
		"mcp__brave__search", "mcp__tavily__search",
		"mcp__kagi__search", "mcp__serpapi__search",
		"read_url", "mcp__jina__read_url", "web_fetch":
		// Globe with meridians — anything that fetches *content* over
		// the web (search results, read URL, web_fetch). User said these
		// all need the obvious "网络" affordance; curl/wget are different
		// (raw HTTP I/O plumbing, kept as 📄).
		return "🌐"
	case "curl", "wget":
		return "📄"
	case "memory_save", "remember", "memory":
		return "🧠"
	case "todo", "todo_write":
		return "📋"
	case "gh", "git":
		return "🔀"
	case "docker":
		return "🐳"
	case "systemctl", "service":
		return "⚙️"
	default:
		return "⚡"
	}
}

// stripHookMessages removes RTK hook interception messages from tool output.
// (Reasonix-specific.)
func stripHookMessages(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "blocked: hook") ||
			strings.Contains(line, "[project/PreToolUse]") ||
			strings.Contains(line, "[project/PostToolUse]") ||
			strings.Contains(line, "[global/PreToolUse]") ||
			strings.Contains(line, "[global/PostToolUse]") ||
			strings.Contains(line, "RTK supports this command") ||
			strings.Contains(line, "Please add 'rtk' prefix") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// isHookOnlyOutput returns true if the output contains only hook block messages.
func isHookOnlyOutput(output string) bool {
	if output == "" {
		return false
	}
	return strings.TrimSpace(stripHookMessages(output)) == ""
}

// stripBackgroundJobs removes reasonix background-job lifecycle blocks from text.
// These are injected by the controller and are not part of the agent's answer.
func stripBackgroundJobs(text string) string {
	// Remove entire <background-jobs>...</background-jobs> block (multi-line).
	for {
		start := strings.Index(text, "<background-jobs>")
		if start < 0 {
			break
		}
		end := strings.Index(text[start:], "</background-jobs>")
		if end < 0 {
			break
		}
		end += start + len("</background-jobs>")
		// Also eat the trailing newline(s) after the closing tag.
		text = text[:start] + strings.TrimLeft(text[end:], "\n")
	}
	return strings.TrimSpace(text)
}

// formatToolArgs extracts a human-readable summary from tool JSON args.
func formatToolArgs(toolName, argsJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return trimUTF8Bytes(argsJSON, 100)
	}
	switch toolName {
	case "grep", "codegraph_search":
		if q, ok := m["pattern"].(string); ok && q != "" && len(q) > 1 {
			return "💬 " + trimUTF8Bytes(q, 80)
		}
		return ""
	case "codegraph_explore":
		return ""
	case "codegraph_callees", "codegraph_callers", "codegraph_impact":
		if q, ok := m["name"].(string); ok && q != "" {
			return "🏷 " + q
		}
	case "codegraph_context":
		if f, ok := m["file"].(string); ok && f != "" {
			line := ""
			if l, ok := m["line"].(float64); ok && l > 0 {
				line = fmt.Sprintf(":%d", int(l))
			}
			return "📄 " + trimUTF8Bytes(f, 60) + line
		}
	case "codegraph_files":
		if p, ok := m["pattern"].(string); ok && p != "" {
			return "📁 " + p
		}
	case "read_file":
		if p, ok := m["path"].(string); ok && p != "" && len(p) > 3 {
			return "📄 " + trimUTF8Bytes(p, 80)
		}
		return ""
	case "write_file", "edit_file", "multi_edit":
		if p, ok := m["path"].(string); ok && p != "" && len(p) > 3 {
			return "📄 " + trimUTF8Bytes(p, 80)
		}
		return ""
	case "bash":
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			return "💻 " + trimUTF8Bytes(cmd, 100)
		}
	case "ls":
		if p, ok := m["path"].(string); ok && p != "" && len(p) > 2 {
			return "📁 " + trimUTF8Bytes(p, 80)
		}
		return ""
	case "glob":
		if p, ok := m["pattern"].(string); ok && p != "" && len(p) > 2 {
			return "📁 " + p
		}
	case "search_web":
		if q, ok := m["query"].(string); ok && q != "" {
			return "🔍 " + trimUTF8Bytes(q, 80)
		}
	case "web_fetch", "read_url":
		if u, ok := m["url"].(string); ok && u != "" {
			return "🌐 " + trimUTF8Bytes(u, 80)
		}
	case "remember":
		if t, ok := m["title"].(string); ok && t != "" {
			return "🧠 " + t
		}
	case "ask":
		return ""
	}
	for _, v := range m {
		if s, ok := v.(string); ok && len(s) > 0 && len(s) < 100 {
			return "💬 " + trimUTF8Bytes(s, 80)
		}
	}
	return ""
}

// formatToolResult formats tool output for compact Telegram display.
func formatToolResult(toolName, output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return "(empty)"
	}
	switch toolName {
	case "ls", "glob", "codegraph_files":
		return formatDirListing(output)
	case "grep", "codegraph_search", "codegraph_trace":
		return formatSearchResult(output)
	}
	return trimUTF8Bytes(output, 300)
}

func formatDirListing(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	var items []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			items = append(items, l)
		}
	}
	if len(items) == 0 {
		return "(empty)"
	}
	if len(items) <= 6 {
		var b strings.Builder
		for i, item := range items {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(item)
		}
		return b.String()
	}
	var b strings.Builder
	cols := 3
	for i, item := range items {
		if i > 0 {
			if i%cols == 0 {
				b.WriteString("\n")
			} else {
				b.WriteString("  ")
			}
		}
		b.WriteString(item)
	}
	total := len(items)
	if total > 30 {
		b.WriteString(fmt.Sprintf("\n… 共 %d 项", total))
	}
	return b.String()
}

func formatSearchResult(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) <= 5 {
		return trimUTF8Bytes(output, 300)
	}
	var b strings.Builder
	for i := 0; i < 3 && i < len(lines); i++ {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(trimUTF8Bytes(lines[i], 80))
	}
	b.WriteString(fmt.Sprintf("\n… 共 %d 条匹配", len(lines)))
	return b.String()
}
