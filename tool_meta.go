package main

import "strings"

type toolMeta struct {
	Label string // 中文描述
	Risk  string // "safe" | "risk"
}

var toolMetaMap = map[string]toolMeta{
	// 🔴 风险
	"bash":            {"执行命令", "risk"},
	"write_file":      {"写入文件", "risk"},
	"edit_file":       {"编辑文件", "risk"},
	"multi_edit":      {"批量编辑", "risk"},
	"move_file":       {"移动文件", "risk"},
	"delete_range":    {"删除文本", "risk"},
	"delete_symbol":   {"删除符号", "risk"},
	"ctx_run":         {"运行上下文", "risk"},
	"task":            {"子代理任务", "risk"},
	"run_skill":       {"运行技能", "risk"},
	"slash_command":   {"斜杠命令", "risk"},
	"explore":         {"代码探索", "risk"},
	"research":        {"研究调查", "risk"},
	"review":          {"代码审查", "risk"},
	"security_review": {"安全审查", "risk"},

	// 🟢 安全
	"note":                  {"记录笔记", "safe"},
	"remember":              {"保存记忆", "safe"},
	"forget":                {"删除记忆", "safe"},
	"audit_finish":          {"完成审计", "safe"},
	"schedule_task":         {"定时任务", "safe"},
	"delete_scheduled_task": {"删除定时任务", "safe"},
	"kill_shell":            {"终止后台", "safe"},
	"install_skill":         {"安装技能", "safe"},
	"install_source":        {"安装来源", "safe"},
	"notebook_edit":         {"编辑笔记本", "safe"},
}

func getToolMeta(toolName string) toolMeta {
	if m, ok := toolMetaMap[toolName]; ok {
		return m
	}
	// MCP 外部插件
	if strings.HasPrefix(toolName, "mcp_") {
		return toolMeta{"外部插件", "risk"}
	}
	// 未知工具兜底
	return toolMeta{toolName, "risk"}
}

// firstLine returns the first line of s, truncated to maxLen runes if longer.
// Used for the approval message parameter summary line.
func firstLine(s string, maxLen int) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		s = s[:idx]
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
