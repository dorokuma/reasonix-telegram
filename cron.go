package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/robfig/cron/v3"
)

type CronTask struct {
	ID      int64        `json:"id"`
	ChatID  int64        `json:"chat_id"`
	Spec    string       `json:"spec"`
	Prompt  string       `json:"prompt"`
	RunOnce bool         `json:"run_once,omitempty"`
	EntryID cron.EntryID `json:"-"`
}

type CronManager struct {
	app             *App
	cron            *cron.Cron
	tasks           map[int64]*CronTask
	nextID          int64
	filePath        string
	mu              sync.Mutex
	watcher         *fsnotify.Watcher
	lastFileContent []byte
}

func (a *App) initCron() {
	c := cron.New(cron.WithParser(cron.NewParser(
		cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow,
	)))

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatalf("cron: failed to create fsnotify watcher: %v", err)
	}

	cm := &CronManager{
		app:      a,
		cron:     c,
		tasks:    make(map[int64]*CronTask),
		nextID:   1,
		filePath: filepath.Join(a.state.dir, "cron_tasks.json"),
		watcher:  watcher,
	}
	a.cronManager = cm

	// Ensure file exists for watching
	if _, err := os.Stat(cm.filePath); os.IsNotExist(err) {
		emptyList := []byte("[]")
		if err := os.WriteFile(cm.filePath, emptyList, 0644); err != nil {
			log.Printf("cron: failed to create initial empty tasks file: %v", err)
		}
	}

	cm.load()
	c.Start()
	log.Printf("Cron scheduler started, loaded tasks from %s", cm.filePath)

	dir := filepath.Dir(cm.filePath)
	if err := watcher.Add(dir); err != nil {
		log.Printf("cron: failed to watch config dir %s: %v", dir, err)
	} else {
		go cm.watchLoop()
	}
}

func (cm *CronManager) saveLocked() error {
	var list []*CronTask
	for _, t := range cm.tasks {
		list = append(list, t)
	}

	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal tasks failed: %w", err)
	}

	cm.lastFileContent = b

	if err := os.WriteFile(cm.filePath, b, 0644); err != nil {
		return fmt.Errorf("write file failed: %w", err)
	}
	return nil
}

func (cm *CronManager) save() {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	if err := cm.saveLocked(); err != nil {
		log.Printf("cron: save failed: %v", err)
	}
}

func (cm *CronManager) load() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	b, err := os.ReadFile(cm.filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		log.Printf("cron: read file failed: %v", err)
		return
	}

	var list []*CronTask
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("cron: unmarshal tasks failed: %v", err)
		return
	}

	cm.lastFileContent = b

	for _, t := range list {
		cm.tasks[t.ID] = t
		if t.ID >= cm.nextID {
			cm.nextID = t.ID + 1
		}

		taskCopy := t
		entryID, err := cm.cron.AddFunc(t.Spec, func() {
			cm.app.triggerCronTask(taskCopy)
		})
		if err != nil {
			log.Printf("cron: reschedule task %d failed: %v", t.ID, err)
			continue
		}
		t.EntryID = entryID
	}
}

func (cm *CronManager) watchLoop() {
	var timer *time.Timer
	const delay = 100 * time.Millisecond

	for {
		select {
		case event, ok := <-cm.watcher.Events:
			if !ok {
				return
			}
			if filepath.Clean(event.Name) == filepath.Clean(cm.filePath) {
				if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
					if timer != nil {
						timer.Stop()
					}
					timer = time.AfterFunc(delay, func() {
						cm.reload()
					})
				}
			}
		case err, ok := <-cm.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("cron: watcher error: %v", err)
		}
	}
}

func (cm *CronManager) reload() {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	b, err := os.ReadFile(cm.filePath)
	if err != nil {
		log.Printf("cron: reload read file failed: %v", err)
		return
	}

	if bytes.Equal(cm.lastFileContent, b) {
		return
	}

	var list []*CronTask
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("cron: reload unmarshal failed: %v", err)
		return
	}

	cm.lastFileContent = b
	cm.clearTasksLocked()

	for _, t := range list {
		cm.tasks[t.ID] = t
		if t.ID >= cm.nextID {
			cm.nextID = t.ID + 1
		}

		taskCopy := t
		entryID, err := cm.cron.AddFunc(t.Spec, func() {
			cm.app.triggerCronTask(taskCopy)
		})
		if err != nil {
			log.Printf("cron: reload reschedule task %d failed: %v", t.ID, err)
			continue
		}
		t.EntryID = entryID
	}
	log.Printf("cron: config reloaded, active tasks count: %d", len(cm.tasks))
}

func (cm *CronManager) clearTasksLocked() {
	for _, t := range cm.tasks {
		cm.cron.Remove(t.EntryID)
	}
	cm.tasks = make(map[int64]*CronTask)
}

func (a *App) triggerCronTask(task *CronTask) {
	srcPath := a.state.sessionPathForChat(task.ChatID)

	succeeded := false
	if task.RunOnce {
		defer func() {
			cm := a.cronManager
			cm.mu.Lock()
			if t, ok := cm.tasks[task.ID]; ok {
				cm.cron.Remove(t.EntryID)
				delete(cm.tasks, task.ID)
				if err := cm.saveLocked(); err != nil {
					log.Printf("cron: run-once task %d auto-delete: save failed: %v", task.ID, err)
				} else {
					log.Printf("cron: run-once task %d auto-deleted (succeeded=%v)", task.ID, succeeded)
				}
			}
			cm.mu.Unlock()
		}()
	}

	tmpFile, err := os.CreateTemp("", fmt.Sprintf("cron_session_%d_*.jsonl", task.ChatID))
	if err != nil {
		log.Printf("cron: failed to create temp file: %v", err)
		a.reply(task.ChatID, "⏰ 定时任务执行失败：创建临时文件失败")
		return
	}
	tmpPath := tmpFile.Name()
	tmpFile.Close()

	defer os.Remove(tmpPath)

	if _, err := os.Stat(srcPath); err == nil {
		if err := copyFile(srcPath, tmpPath); err != nil {
			log.Printf("cron: failed to copy session file: %v", err)
			a.reply(task.ChatID, "⏰ 定时任务执行失败：克隆会话文件失败")
			return
		}
	} else {
		f, err := os.Create(tmpPath)
		if err != nil {
			log.Printf("cron: failed to create empty session: %v", err)
			return
		}
		f.Close()
	}

	a.reply(task.ChatID, fmt.Sprintf("⏰ 定时任务触发：正在执行 \"%s\"...", task.Prompt))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// 注入当前系统时间，避免 LLM 使用训练截止日期
	now := time.Now()
	weekdayMap := map[time.Weekday]string{
		time.Sunday: "周日", time.Monday: "周一", time.Tuesday: "周二",
		time.Wednesday: "周三", time.Thursday: "周四", time.Friday: "周五",
		time.Saturday: "周六",
	}
	datePrefix := fmt.Sprintf("[系统时间：%s]\n", now.Format("2006年1月2日")+" "+weekdayMap[now.Weekday()])
	fullPrompt := datePrefix + task.Prompt

	cmd := exec.CommandContext(ctx, a.cfg.ReasonixBin, "run", "--resume", tmpPath, "--model", "deepseek-v4-flash", fullPrompt)
	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("cron: command exec failed: %v, output: %s", err, string(out))
		a.reply(task.ChatID, fmt.Sprintf("⏰ 定时任务执行失败：%v\n输出：%s", err, string(out)))
		return
	}

	result := string(out)
	result = stripANSI(result)
	result = stripThinkBlocks(result)

	// Filter noise lines (token stats, status dots, thinking bars, etc.)
	lines := strings.Split(result, "\n")
	var cleanLines []string
	for _, line := range lines {
		if !isReasonixNoise(strings.TrimSpace(line)) {
			cleanLines = append(cleanLines, line)
		}
	}
	result = strings.Join(cleanLines, "\n")
	result = stripErrorLines(result)

	if strings.TrimSpace(result) == "" {
		result = "(任务执行完毕，无输出内容)"
	}

	a.sendTextParts(task.ChatID, result, nil)
	succeeded = true
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}


func parseCronCmd(args string) (spec string, prompt string, err error) {
	fields := strings.Fields(args)
	if len(fields) < 6 {
		return "", "", fmt.Errorf("格式错误。用法：/cron [分] [时] [日] [月] [周] [Prompt]\n例如：/cron */5 * * * * 总结今天的工作")
	}
	spec = strings.Join(fields[:5], " ")
	prompt = strings.Join(fields[5:], " ")
	return spec, prompt, nil
}

func (a *App) handleCron(m *tgbotapi.Message, args string) {
	spec, prompt, err := parseCronCmd(args)
	if err != nil {
		a.reply(m.Chat.ID, "❌ "+err.Error())
		return
	}

	_, err = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow).Parse(spec)
	if err != nil {
		a.reply(m.Chat.ID, fmt.Sprintf("❌ 无效的 cron 表达式：%v", err))
		return
	}

	cm := a.cronManager
	cm.mu.Lock()
	taskID := cm.nextID
	cm.nextID++

	task := &CronTask{
		ID:     taskID,
		ChatID: m.Chat.ID,
		Spec:   spec,
		Prompt: prompt,
	}

	entryID, err := cm.cron.AddFunc(spec, func() {
		a.triggerCronTask(task)
	})
	if err != nil {
		cm.mu.Unlock()
		a.reply(m.Chat.ID, fmt.Sprintf("❌ 添加定时任务失败：%v", err))
		return
	}

	task.EntryID = entryID
	cm.tasks[taskID] = task
	if err := cm.saveLocked(); err != nil {
		log.Printf("cron: save after add task %d failed: %v", taskID, err)
	}
	cm.mu.Unlock()

	a.reply(m.Chat.ID, fmt.Sprintf("✅ 定时任务添加成功！\n任务 ID: %d\n表达式: `%s`\nPrompt: %s", task.ID, task.Spec, task.Prompt))
}

func (a *App) handleCronList(m *tgbotapi.Message) {
	cm := a.cronManager
	cm.mu.Lock()
	defer cm.mu.Unlock()

	var sb strings.Builder
	sb.WriteString("**📅 定时任务列表**\n\n")

	count := 0
	for _, t := range cm.tasks {
		if t.ChatID == m.Chat.ID {
			sb.WriteString(fmt.Sprintf("ID: %d\n表达式: `%s`\nPrompt: %s\n\n", t.ID, t.Spec, t.Prompt))
			count++
		}
	}

	if count == 0 {
		a.reply(m.Chat.ID, "当前聊天没有设定任何定时任务。")
		return
	}

	a.reply(m.Chat.ID, sb.String())
}

func (a *App) handleCronDel(m *tgbotapi.Message, args string) {
	id, err := strconv.ParseInt(args, 10, 64)
	if err != nil {
		a.reply(m.Chat.ID, "❌ 任务 ID 必须是数字")
		return
	}

	cm := a.cronManager
	cm.mu.Lock()
	task, ok := cm.tasks[id]
	if !ok || task.ChatID != m.Chat.ID {
		cm.mu.Unlock()
		a.reply(m.Chat.ID, fmt.Sprintf("❌ 找不到任务 ID 为 %d 的定时任务", id))
		return
	}

	cm.cron.Remove(task.EntryID)
	delete(cm.tasks, id)
	if err := cm.saveLocked(); err != nil {
		log.Printf("cron: save after delete task %d failed: %v", id, err)
	}
	cm.mu.Unlock()

	a.reply(m.Chat.ID, fmt.Sprintf("✅ 定时任务 %d 已成功删除", id))
}
