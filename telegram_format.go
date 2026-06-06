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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// _MDV2_ESCAPE_RE matches characters that need escaping in Telegram MarkdownV2.
var _MDV2_ESCAPE_RE = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#\+\-=|{}.!\\])`)

func _escape_mdv2(s string) string {
	return _MDV2_ESCAPE_RE.ReplaceAllString(s, `\$1`)
}

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
	// Bold: **text** (content cannot contain *, avoids *** conflicts)
	reBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
	// Italic: *text* (single asterisk, not across newlines)
	// Also handles *text** (model sometimes outputs two closing asterisks)
	reItalic = regexp.MustCompile(`\*([^*\n]+)\*(\*)?`)
	// Strikethrough: ~~text~~
	reStrike = regexp.MustCompile(`~~(.+?)~~`)
	// Spoiler: ||text||
	reSpoiler = regexp.MustCompile(`\|\|(.+?)\|\|`)
	// Unordered list item: - or * at line start
	reUlItem = regexp.MustCompile(`(?m)^\s*[-*+]\s+(.+)$`)
	// Ordered list item: 1. at line start (skip lines with | — those are table rows)
	reOlItem = regexp.MustCompile(`(?m)^\s*\d+\.\s+(.+)$`)
	// Blockquote: > at line start
	reBlockquote = regexp.MustCompile(`(?m)^>\s?(.*)$`)
	// Horizontal rule: --- / *** / ___
	reHr = regexp.MustCompile(`(?m)^\s*[-*_]{3,}\s*$`)
)

// reTableSep matches a GFM table delimiter row: |---|---|
var reTableSep = regexp.MustCompile(`^\s*\|?\s*:?-+:?\s*(?:\|\s*:?-+:?\s*){1,}\|?\s*$`)

// wrapMarkdownTables rewrites GFM pipe tables into Telegram-friendly
// bold-heading + bullet groups. Based on Hermes _wrap_markdown_tables.
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

		// Check if this line starts a table block: has '|' AND next line is a separator.
		if !strings.Contains(line, "|") || i+1 >= len(lines) || !reTableSep.MatchString(lines[i+1]) {
			out = append(out, line)
			i++
			continue
		}

		// Consume the table block: header, separator, then data rows.
		header := line
		sepLine := lines[i+1] // delimiter row (skipped)
		_ = sepLine
		var dataRows []string
		j := i + 2
		for j < len(lines) {
			row := lines[j]
			rowStripped := strings.TrimSpace(row)
			if rowStripped == "" || !strings.Contains(row, "|") {
				break
			}
			dataRows = append(dataRows, row)
			j++
		}

		rendered := renderTableBlock(header, dataRows)
		out = append(out, rendered)
		i = j
	}
	return strings.Join(out, "\n")
}

// splitTableRow splits a GFM table row into stripped cell values.
func splitTableRow(line string) []string {
	s := strings.TrimSpace(line)
	if strings.HasPrefix(s, "|") {
		s = s[1:]
	}
	if strings.HasSuffix(s, "|") {
		s = s[:len(s)-1]
	}
	var cells []string
	for _, cell := range strings.Split(s, "|") {
		cells = append(cells, strings.TrimSpace(cell))
	}
	return cells
}

// renderTableBlock converts a table header + data rows to bold + bullet format.
func renderTableBlock(headerLine string, dataRows []string) string {
	headers := splitTableRow(headerLine)
	if len(headers) < 2 || len(dataRows) == 0 {
		return headerLine + "\n" + strings.Join(dataRows, "\n")
	}

	// Detect row-label column: present when data rows have one more cell.
	firstCells := splitTableRow(dataRows[0])
	hasRowLabel := len(firstCells) == len(headers)+1

	var groups []string
	for _, row := range dataRows {
		cells := splitTableRow(row)
		var heading string
		var dataCells []string

		if hasRowLabel {
			heading = cells[0]
			if heading == "" {
				heading = "Row"
			}
			if len(cells) > 1 {
				dataCells = cells[1:]
			}
		} else {
			heading = ""
			for _, c := range cells {
				if c != "" {
					heading = c
					break
				}
			}
			if heading == "" {
				heading = "Row"
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

		// Build bullets. Skip any bullet whose value duplicates the heading.
		var bullets []string
		for idx, val := range dataCells {
			if !hasRowLabel && val == heading {
				continue
			}
			header := ""
			if idx < len(headers) {
				header = headers[idx]
			}
			bullets = append(bullets, "• "+header+": "+val)
		}

		group := heading
		for _, b := range bullets {
			group += "\n" + b
		}
		groups = append(groups, group)
	}

	return strings.Join(groups, "\n\n")
}

// formatForTelegram converts standard markdown to Telegram-compatible HTML.
// Safe to call with any model output; non-markdown text is HTML-escaped.
func formatForTelegram(content string) string {
	if content == "" {
		return content
	}
	content = stripHookMessages(content)
	if content == "" {
		return ""
	}

	// Hermes-style placeholder: \x00PH{counter}\x00
	// Null bytes survive Telegram's text processing.
	// Ordered slice + map: insertion order tracked so restoration runs in
	// reverse order (same as Hermes), guaranteeing nested placeholders resolve.
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

	// Inline code: extracted to list, NOT using ph().
	type codeEntry struct{ html string }
	var inlineCodes []codeEntry
	extractInlineCode := func(html string) string {
		idx := len(inlineCodes)
		inlineCodes = append(inlineCodes, codeEntry{html: html})
		return fmt.Sprintf("§%d§", idx)
	}

	text := content

	// 0a) Horizontal rules
	text = reHr.ReplaceAllString(text, "\n━━━━━━━━━━━━━━\n")
	// 0b) GFM pipe tables
	text = wrapMarkdownTables(text)
	// 0c) Strip LaTeX delimiters
	text = stripLatexDelimiters(text)

	// 1) Protect fenced code blocks
	text = reFenced.ReplaceAllStringFunc(text, func(match string) string {
		inner := match
		if idx := strings.Index(inner, "\n"); idx >= 0 {
			_ = strings.TrimSpace(inner[3:idx])
			inner = inner[idx+1:]
		} else {
			inner = inner[3:]
		}
		if strings.HasSuffix(inner, "```") {
			inner = inner[:len(inner)-3]
		}
		inner = strings.TrimRight(inner, "\n")
		inner = htmlEscape(inner)
		return ph("<pre>" + inner + "</pre>")
	})

	// 2) Extract inline code
	text = reInlineCode.ReplaceAllStringFunc(text, func(match string) string {
		inner := match[1 : len(match)-1]
		return extractInlineCode("<code>" + htmlEscape(inner) + "</code>")
	})

	// 3) Links
	text = reLink.ReplaceAllStringFunc(text, func(match string) string {
		parts := reLink.FindStringSubmatch(match)
		if len(parts) < 3 {
			return match
		}
		display := htmlEscape(parts[1])
		rawURL := parts[2]
		if strings.HasPrefix(strings.ToLower(rawURL), "javascript:") || strings.HasPrefix(strings.ToLower(rawURL), "data:") {
			return display
		}
		url := htmlEscape(rawURL)
		return ph("<a href=\"" + url + "\">" + display + "</a>")
	})

	// 4) Headers
	text = reHeader.ReplaceAllStringFunc(text, func(match string) string {
		parts := reHeader.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		inner := strings.TrimSpace(parts[1])
		inner = strings.ReplaceAll(inner, "**", "")
		inner = strings.ReplaceAll(inner, "__", "")
		inner = strings.ReplaceAll(inner, "*", "")
		return ph("<b>" + htmlEscape(inner) + "</b>")
	})

	// 5) Bold — content cannot contain * (avoids conflicts with *** and italic)
	text = reBold.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBold.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		if strings.Contains(parts[1], "§") {
			return match
		}
		return ph("<b>" + htmlEscape(parts[1]) + "</b>")
	})

	// 6) Italic
	text = reItalic.ReplaceAllStringFunc(text, func(match string) string {
		parts := reItalic.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<i>" + htmlEscape(parts[1]) + "</i>")
	})


	// 7) Strikethrough
	text = reStrike.ReplaceAllStringFunc(text, func(match string) string {
		parts := reStrike.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<s>" + htmlEscape(parts[1]) + "</s>")
	})

	// 8) Spoiler
	text = reSpoiler.ReplaceAllStringFunc(text, func(match string) string {
		parts := reSpoiler.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph(`<span class="tg-spoiler">` + htmlEscape(parts[1]) + `</span>`)
	})

	// 9) Unordered list
	text = reUlItem.ReplaceAllStringFunc(text, func(match string) string {
		parts := reUlItem.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("• " + htmlEscape(strings.TrimSpace(parts[1])))
	})

	// 10) Ordered list (skip table rows with |)
	text = reOlItem.ReplaceAllStringFunc(text, func(match string) string {
		if strings.Contains(match, "|") {
			return match
		}
		parts := reOlItem.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph(htmlEscape(strings.TrimSpace(parts[1])))
	})

	// 11) Blockquotes
	text = reBlockquote.ReplaceAllStringFunc(text, func(match string) string {
		parts := reBlockquote.FindStringSubmatch(match)
		if len(parts) < 2 {
			return match
		}
		return ph("<blockquote>" + htmlEscape(strings.TrimSpace(parts[1])) + "</blockquote>")
	})

	// 12) HTML-escape remaining plain text
	text = htmlEscape(text)

	// 13) Restore ph() placeholders in reverse insertion order (Hermes-style)
	//     Ordered slice guarantees nested placeholders resolve correctly.
	for i := len(phOrder) - 1; i >= 0; i-- {
		text = strings.ReplaceAll(text, phOrder[i], placeholders[phOrder[i]])
	}

	// 14) Restore inline code
	for i, entry := range inlineCodes {
		text = strings.ReplaceAll(text, fmt.Sprintf("§%d§", i), entry.html)
	}

	return text
}

	// Placeholder system for fenced code blocks, links, headers, bold, italic, etc.
	// Uses «N» delimiters that survive Telegram.
// reLatexDisplay and reLatexInline match LaTeX math delimiters.
var (
	reLatexDisplay = regexp.MustCompile(`\\\[([\s\S]*?)\\\]`)
	reLatexInline  = regexp.MustCompile(`\\\(([\s\S]*?)\\\)`)
)

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
	case "codegraph_callees", "codegraph_callers", "codegraph_context", "codegraph_explore", "codegraph_trace", "codegraph_impact":
		return "🔍"
	case "ask":
		return "❓"
	case "search_web", "web_search",
		"mcp__jina__search_web", "mcp__jina__search",
		"mcp__brave__search", "mcp__tavily__search",
		"mcp__kagi__search", "mcp__serpapi__search":
		// Globe with meridians — the canonical "the web" symbol. User found
		// this far more recognizable than the spider-web alternative.
		return "🌐"

	case "read_url", "mcp__jina__read_url":
		// Same family as web_fetch — fetch a URL and read it. Share the
		// "📄" emoji with curl/wget/web_fetch (network I/O) so the user
		// sees a consistent network category.
		return "📄"
	case "curl", "wget", "web_fetch":
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

// stripHookMessages removes RTK hook interception messages from tool output.
func stripHookMessages(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, "blocked: hook") || strings.Contains(line, "[project/PreToolUse]") || strings.Contains(line, "RTK supports this command") || strings.Contains(line, "Please add 'rtk' prefix") {
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

// formatToolArgs extracts a human-readable summary from tool JSON args.
func formatToolArgs(toolName, argsJSON string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &m); err != nil {
		return trimUTF8Bytes(argsJSON, 100)
	}
	// Extract the most meaningful field per tool type.
	switch toolName {
	case "grep", "codegraph_search":
		if q, ok := m["pattern"].(string); ok && q != "" && len(q) > 1 {
			return "💬 " + trimUTF8Bytes(q, 80)
		}
		return ""
	case "codegraph_explore":
		// No args usually
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
		return ""
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
		// Don't show args for ask tool
		return ""
	}
	// Fallback: show first short string value.
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
	// Directory listing tools: format as compact grid.
	switch toolName {
	case "ls", "glob", "codegraph_files":
		return formatDirListing(output)
	case "grep", "codegraph_search", "codegraph_trace":
		return formatSearchResult(output)
	}
	// General: truncate with ellipsis.
	return trimUTF8Bytes(output, 300)
}

// formatDirListing renders a vertical file list as a compact 2-3 column grid.
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
		// Few items: simple bullet list.
		var b strings.Builder
		for i, item := range items {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(item)
		}
		return b.String()
	}
	// Many items: compact grid, 3 columns.
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

// formatSearchResult formats grep/search output for compact display.
func formatSearchResult(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) <= 5 {
		return trimUTF8Bytes(output, 300)
	}
	// Show first 3 matches + count.
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
