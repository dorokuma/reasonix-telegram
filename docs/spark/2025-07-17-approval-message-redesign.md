# 审批消息重设计

**日期**: 2025-07-17
**状态**: 已确认

## 目标

重新设计 Telegram 桥的审批消息呈现方式，解决当前消息视觉单薄、信息密度低的问题。

## 当前问题

- 审批消息为一段平文本 + 竖排按钮
- 工具名直接展示英文原始名称
- 无风险分级提示
- 参数预览混杂在文本中，缺乏结构化

## 设计方案

### 消息结构

```
{emoji} {中文描述}

📁 {关键参数摘要}

```json
{完整参数预览}
```

[按钮...]
```

### 标题行

格式 `{emoji} {中文描述}`，用 emoji 区分风险等级：

- 🟢 安全级 — 仅影响 Agent 自身状态（记忆、笔记、定时任务等）
- 🔴 风险级 — 修改文件、执行命令、影响系统

### 参数区

- 标题下一行：📁 前缀 + 关键参数摘要（文件路径、命令首行等），一行内完成
- 代码块：完整参数 JSON，用 ``` 包裹，语言标注为 json

### 按钮

- 保持现有竖排布局（每行一个按钮）
- 文案不加宽空格，自然文字
- task scope：`[✅ 批准]` `[❌ 拒绝]`
- gate scope：`[✅ 批准一次]` `[🔒 始终批准]` `[❌ 拒绝此次]`

## 工具分类

### 🔴 风险

| 工具名 | 中文描述 |
|---|---|
| `bash` | 执行命令 |
| `write_file` | 写入文件 |
| `edit_file` | 编辑文件 |
| `multi_edit` | 批量编辑 |
| `move_file` | 移动文件 |
| `delete_range` | 删除文本 |
| `delete_symbol` | 删除符号 |
| `ctx_run` | 运行上下文 |
| `task` | 子代理任务 |
| `run_skill` | 运行技能 |
| `slash_command` | 斜杠命令 |
| `explore` | 代码探索 |
| `research` | 研究调查 |
| `review` | 代码审查 |
| `security_review` | 安全审查 |
| `mcp_*` | 外部插件（兜底） |

### 🟢 安全

| 工具名 | 中文描述 |
|---|---|
| `note` | 记录笔记 |
| `remember` | 保存记忆 |
| `forget` | 删除记忆 |
| `audit_finish` | 完成审计 |
| `schedule_task` | 定时任务 |
| `delete_scheduled_task` | 删除定时任务 |
| `kill_shell` | 终止后台 |
| `install_skill` | 安装技能 |
| `install_source` | 安装来源 |
| `notebook_edit` | 编辑笔记本 |

## 实现要点

### 涉及文件

| 文件 | 改动 |
|---|---|
| `stream.go` | 重写 `onApprovalRequest` 中的消息模板和按钮定义 |
| 新增 `tool_meta.go` | 工具名→中文描述+风险等级的映射表 |

### 映射函数

```go
type toolMeta struct {
    Label string // 中文描述
    Risk  string // "safe" | "risk"
}

func getToolMeta(toolName string) toolMeta
```

- 精确匹配优先
- `mcp_` 前缀 → 中文描述「外部插件」，风险级
- 未知工具 → 兜底使用原始 toolName，🔴 风险级

### 消息构建

1. 查 `getToolMeta(toolName)` 得 emoji + 中文描述
2. 构建标题：`{emoji} {label}`
3. 提取参数摘要（取 preview 首行或截断至 100 字符）
4. 完整参数放入 code block
5. 按 scope 选择按钮组

### 按钮变更

- task: 文案从 `✅ 批准` → `✅ 批准`（不变），`❌ 拒绝` → `❌ 拒绝`（不变）
- gate: 文案不变（`✅ 批准一次` `🔒 始终批准` `❌ 拒绝此次`），仅去掉可能的空格加宽

## 示例

### 风险操作（gate scope）

```
🔴 写入文件

📁 /etc/config.json

```json
{"host": "0.0.0.0", "port": 22}
```

[✅ 批准一次]
[🔒 始终批准]
[❌ 拒绝此次]
```

### 安全操作（gate scope）

```
🟢 保存记忆

📁 用户偏好设置

```json
{"key": "theme", "value": "dark"}
```

[✅ 批准一次]
[🔒 始终批准]
[❌ 拒绝此次]
```

### 风险操作（task scope）

```
🔴 子代理任务

📁 探索代码库结构

```json
{"prompt": "find all usages of...", "subagent": "explore"}
```

[✅ 批准]
[❌ 拒绝]
```
