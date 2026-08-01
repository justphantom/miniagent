package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

type todoItem struct {
	ID      int    `json:"id"`
	Subject string `json:"subject"`
	Status  string `json:"status"` // pending | in_progress | completed
}

// TaskList 是进程内任务清单，sync.Mutex 保护——工具并行执行（见 runToolsParallel）。
// 不进 session：过程态，跨会话落盘无意义。
type TaskList struct {
	mu    sync.Mutex
	items []todoItem
	next  int
}

// TodoTool 返回管理 TaskList 的工具。action: add|update|list|complete|delete。
func TodoTool(tasks *TaskList) Tool {
	return Tool{
		Name:        "todo",
		Description: "管理任务清单（进程内，不落盘）。action: add(新增,返回 id)|update(改 status/subject)|list(列出全部)|complete(标记完成)|delete(删除)。status: pending|in_progress|completed。复杂任务先 add 拆解再逐步 complete 推进。",
		Parameters: object(map[string]any{
			"action":  map[string]any{"type": "string", "description": "add|update|list|complete|delete"},
			"id":      map[string]any{"type": "integer", "description": "update/complete/delete 时指定任务 id"},
			"subject": map[string]any{"type": "string", "description": "add 时任务标题；update 时新标题（可选）"},
			"status":  map[string]any{"type": "string", "description": "update 时新状态：pending|in_progress|completed"},
		}, "action"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			return runTodo(tasks, args)
		},
	}
}

func runTodo(tasks *TaskList, args string) ToolResult {
	var a struct {
		Action  string `json:"action"`
		ID      int    `json:"id"`
		Subject string `json:"subject"`
		Status  string `json:"status"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	tasks.mu.Lock()
	defer tasks.mu.Unlock()
	switch a.Action {
	case "add":
		if strings.TrimSpace(a.Subject) == "" {
			return ToolResult{IsError: true, Output: "参数缺失：subject"}
		}
		tasks.next++
		it := todoItem{ID: tasks.next, Subject: strings.TrimSpace(a.Subject), Status: "pending"}
		tasks.items = append(tasks.items, it)
		return ToolResult{Output: fmt.Sprintf("已新增任务 id=%d：%s", it.ID, it.Subject)}
	case "update":
		it, ok := findTodo(tasks, a.ID)
		if !ok {
			return ToolResult{IsError: true, Output: fmt.Sprintf("未知任务 id=%d", a.ID)}
		}
		if strings.TrimSpace(a.Subject) != "" {
			it.Subject = strings.TrimSpace(a.Subject)
		}
		if a.Status != "" {
			if !validTodoStatus(a.Status) {
				return ToolResult{IsError: true, Output: fmt.Sprintf("非法 status %q（pending|in_progress|completed）", a.Status)}
			}
			it.Status = a.Status
		}
		return ToolResult{Output: fmt.Sprintf("已更新任务 id=%d：%s [%s]", it.ID, it.Subject, it.Status)}
	case "complete":
		it, ok := findTodo(tasks, a.ID)
		if !ok {
			return ToolResult{IsError: true, Output: fmt.Sprintf("未知任务 id=%d", a.ID)}
		}
		it.Status = "completed"
		return ToolResult{Output: fmt.Sprintf("已完成任务 id=%d：%s", it.ID, it.Subject)}
	case "delete":
		idx := -1
		for i := range tasks.items {
			if tasks.items[i].ID == a.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			return ToolResult{IsError: true, Output: fmt.Sprintf("未知任务 id=%d", a.ID)}
		}
		tasks.items = append(tasks.items[:idx], tasks.items[idx+1:]...)
		return ToolResult{Output: fmt.Sprintf("已删除任务 id=%d", a.ID)}
	case "list":
		if len(tasks.items) == 0 {
			return ToolResult{Output: "无任务"}
		}
		var sb strings.Builder
		for _, it := range tasks.items {
			fmt.Fprintf(&sb, "%d [%s] %s\n", it.ID, it.Status, it.Subject)
		}
		return ToolResult{Output: strings.TrimRight(sb.String(), "\n")}
	default:
		return ToolResult{IsError: true, Output: fmt.Sprintf("未知 action %q（add|update|list|complete|delete）", a.Action)}
	}
}

func findTodo(tasks *TaskList, id int) (*todoItem, bool) {
	for i := range tasks.items {
		if tasks.items[i].ID == id {
			return &tasks.items[i], true
		}
	}
	return nil, false
}

func validTodoStatus(s string) bool {
	return s == "pending" || s == "in_progress" || s == "completed"
}
