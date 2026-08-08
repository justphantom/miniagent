package tools

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// TodoStatus 任务状态。
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// validTodoStatus 校验 status 枚举（空串合法=不改）。
func validTodoStatus(s TodoStatus) bool {
	return s == "" || s == TodoPending || s == TodoInProgress || s == TodoCompleted
}

// TodoItem 单条任务。
type TodoItem struct {
	ID          int        `json:"id"`
	Subject     string     `json:"subject"`
	Description string     `json:"description,omitempty"`
	Status      TodoStatus `json:"status"`
}

// TodoList 任务列表（runToolsParallel 并行安全）：单 Run 内存，跨 step 共享、跨 Run 重置
// （每轮 buildTools 新建）。状态在闭包、不进 transcript，与压缩解耦；LLM 经 todo_list 读最新态。
type TodoList struct {
	mu    sync.Mutex
	items []*TodoItem
	next  int // 自增 ID
}

// Create 新建 pending 任务，返回新 item 副本。
func (l *TodoList) Create(subject, description string) TodoItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	it := &TodoItem{ID: l.next, Subject: subject, Description: description, Status: TodoPending}
	l.items = append(l.items, it)
	return *it
}

// Update 更新 id 任务（字段空跳过），返回更新后 item 副本；id 不存在 → ok=false。
func (l *TodoList) Update(id int, status TodoStatus, subject, description string) (TodoItem, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, it := range l.items {
		if it.ID == id {
			if status != "" {
				it.Status = status
			}
			if subject != "" {
				it.Subject = subject
			}
			if description != "" {
				it.Description = description
			}
			return *it, true
		}
	}
	return TodoItem{}, false
}

// List 返回所有任务副本（按 ID 升序）。
func (l *TodoList) List() []TodoItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TodoItem, len(l.items))
	for i, it := range l.items {
		out[i] = *it
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// TodoTools 返回 create/update/list 三工具，共享 l（单 Run 内存）。
// 工具结果进 transcript（短文本、非 read/shell/write/edit，strip 阶段不碰，完整保留供后续步 LLM 读）。
func TodoTools(l *TodoList) []miniagent.Tool {
	create := miniagent.Tool{
		Name:        "todo_create",
		Description: "新建一条任务（status=pending）。多步任务用它跟踪进度。",
		Parameters: object(map[string]any{
			"subject":     map[string]any{"type": "string", "description": "任务标题（简短祈使句）"},
			"description": map[string]any{"type": "string", "description": "任务详情（可选）"},
		}, "subject"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				Subject     string `json:"subject"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if strings.TrimSpace(a.Subject) == "" {
				return miniagent.ToolResult{IsError: true, Output: "subject 必填"}
			}
			it := l.Create(strings.TrimSpace(a.Subject), strings.TrimSpace(a.Description))
			b, _ := json.Marshal(it)
			return miniagent.ToolResult{Output: string(b)}
		},
	}

	update := miniagent.Tool{
		Name:        "todo_update",
		Description: "更新任务（id 必填；status: pending|in_progress|completed；subject/description 可选，空跳过）。",
		Parameters: object(map[string]any{
			"id":          map[string]any{"type": "integer", "description": "任务 id"},
			"status":      map[string]any{"type": "string", "description": "新状态：pending|in_progress|completed"},
			"subject":     map[string]any{"type": "string", "description": "新标题（可选）"},
			"description": map[string]any{"type": "string", "description": "新详情（可选）"},
		}, "id"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				ID          int    `json:"id"`
				Status      string `json:"status"`
				Subject     string `json:"subject"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if a.ID == 0 {
				return miniagent.ToolResult{IsError: true, Output: "id 必填"}
			}
			status := TodoStatus(a.Status)
			if !validTodoStatus(status) {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("status %q 非法（pending|in_progress|completed）", a.Status)}
			}
			it, ok := l.Update(a.ID, status, strings.TrimSpace(a.Subject), strings.TrimSpace(a.Description))
			if !ok {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("任务 id %d 不存在", a.ID)}
			}
			b, _ := json.Marshal(it)
			return miniagent.ToolResult{Output: string(b)}
		},
	}

	list := miniagent.Tool{
		Name:        "todo_list",
		Description: "列出所有任务及状态（#id [status] subject）。多步任务用它回顾进度。",
		Parameters:  object(map[string]any{}),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			items := l.List()
			if len(items) == 0 {
				return miniagent.ToolResult{Output: "（无任务）"}
			}
			var b strings.Builder
			for _, it := range items {
				fmt.Fprintf(&b, "#%d [%s] %s\n", it.ID, it.Status, it.Subject)
				if it.Description != "" {
					fmt.Fprintf(&b, "    %s\n", it.Description)
				}
			}
			return miniagent.ToolResult{Output: strings.TrimRight(b.String(), "\n")}
		},
	}

	return []miniagent.Tool{create, update, list}
}
