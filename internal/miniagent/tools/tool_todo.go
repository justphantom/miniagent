package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"sort"
	"strings"
	"sync"
)

// TodoStatus is the task status.
type TodoStatus string

const (
	TodoPending    TodoStatus = "pending"
	TodoInProgress TodoStatus = "in_progress"
	TodoCompleted  TodoStatus = "completed"
)

// validTodoStatus validates the status enum (empty string is valid = no change).
func validTodoStatus(s TodoStatus) bool {
	return s == "" || s == TodoPending || s == TodoInProgress || s == TodoCompleted
}

// TodoItem is a single task.
type TodoItem struct {
	ID          int        `json:"id"`
	Subject     string     `json:"subject"`
	Description string     `json:"description,omitempty"`
	Status      TodoStatus `json:"status"`
}

// TodoList is the task list (safe for runToolsParallel): in-memory for a single Run, shared across steps,
// reset across Runs (rebuilt each turn via buildTools). State lives in the closure, never enters the transcript,
// decoupled from compaction; the LLM reads the latest state via todo_list.
type TodoList struct {
	mu    sync.Mutex
	items []*TodoItem
	next  int // auto-increment ID
}

// Create creates a new pending task and returns a copy of the new item.
func (l *TodoList) Create(subject, description string) TodoItem {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	it := &TodoItem{ID: l.next, Subject: subject, Description: description, Status: TodoPending}
	l.items = append(l.items, it)
	return *it
}

// Update updates the task with the given id (empty fields are skipped) and returns a copy of the
// updated item; if the id does not exist → ok=false.
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

// List returns copies of all tasks (ascending by ID).
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

// TodoTools returns the three tools create/update/list, sharing l (in-memory for a single Run).
// Tool results enter the transcript (short text, not read/shell/write/edit, untouched by the strip stage,
// fully preserved for subsequent steps' LLM to read).
func TodoTools(l *TodoList) []miniagent.Tool {
	create := miniagent.Tool{
		Name:        "todo_create",
		Description: "Create a new task (status=pending). Use it to track progress on multi-step tasks.",
		Parameters: object(map[string]any{
			"subject":     map[string]any{"type": "string", "description": "Task title (short imperative sentence)"},
			"description": map[string]any{"type": "string", "description": "Task details (optional)"},
		}, "subject"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				Subject     string `json:"subject"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("parameter parse failed: %v (received %q)", err, args)}
			}
			if strings.TrimSpace(a.Subject) == "" {
				return miniagent.ToolResult{IsError: true, Output: "subject is required"}
			}
			it := l.Create(strings.TrimSpace(a.Subject), strings.TrimSpace(a.Description))
			b, _ := json.Marshal(it)
			return miniagent.ToolResult{Output: string(b)}
		},
	}

	update := miniagent.Tool{
		Name:        "todo_update",
		Description: "Update a task (id required; status: pending|in_progress|completed; subject/description optional, empty skipped).",
		Parameters: object(map[string]any{
			"id":          map[string]any{"type": "integer", "description": "Task id"},
			"status":      map[string]any{"type": "string", "description": "New status: pending|in_progress|completed"},
			"subject":     map[string]any{"type": "string", "description": "New title (optional)"},
			"description": map[string]any{"type": "string", "description": "New details (optional)"},
		}, "id"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			var a struct {
				ID          int    `json:"id"`
				Status      string `json:"status"`
				Subject     string `json:"subject"`
				Description string `json:"description"`
			}
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("parameter parse failed: %v (received %q)", err, args)}
			}
			if a.ID == 0 {
				return miniagent.ToolResult{IsError: true, Output: "id is required"}
			}
			status := TodoStatus(a.Status)
			if !validTodoStatus(status) {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("status %q is invalid (pending|in_progress|completed)", a.Status)}
			}
			it, ok := l.Update(a.ID, status, strings.TrimSpace(a.Subject), strings.TrimSpace(a.Description))
			if !ok {
				return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("task id %d does not exist", a.ID)}
			}
			b, _ := json.Marshal(it)
			return miniagent.ToolResult{Output: string(b)}
		},
	}

	list := miniagent.Tool{
		Name:        "todo_list",
		Description: "List all tasks and their statuses (#id [status] subject). Use it to review progress on multi-step tasks.",
		Parameters:  object(map[string]any{}),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			items := l.List()
			if len(items) == 0 {
				return miniagent.ToolResult{Output: "(no tasks)"}
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
