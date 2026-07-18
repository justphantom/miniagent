package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// MemoryTools returns the four memory tools bound to chatID.
func MemoryTools(store *FactStore, chatID string) []Tool {
	if store == nil {
		return nil
	}
	return []Tool{
		memorySetTool(store, chatID),
		memoryGetTool(store, chatID),
		memoryListTool(store, chatID),
		memoryDeleteTool(store, chatID),
	}
}

func memorySetTool(store *FactStore, chatID string) Tool {
	return Tool{
		Name:        "memory_set",
		Description: "保存一个长期记忆事实。当用户明确说'记住'、你观察到稳定偏好、或想跨会话保留关键上下文时使用。scope 默认 chat（仅当前会话可见），也可选 project（同项目共享）或 global（所有会话共享）。",
		Parameters: object(map[string]any{
			"key":   map[string]any{"type": "string", "description": "事实的短标识符，使用小写英文点号分隔，例如 user.language、project.framework、task.pending_review。"},
			"value": map[string]any{"type": "string", "description": "事实内容，保持简洁。"},
			"scope": map[string]any{"type": "string", "enum": []string{"chat", "project", "global"}, "description": "可见范围：chat=当前会话，project=同工作目录项目共享，global=所有会话。默认 chat。"},
		}, "key", "value"),
		Call: func(_ context.Context, args string) ToolResult {
			var p struct {
				Key   string `json:"key"`
				Value string `json:"value"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return ToolResult{IsError: true, Output: "参数解析失败：" + err.Error()}
			}
			if p.Key == "" {
				return ToolResult{IsError: true, Output: "key 不能为空"}
			}
			scope := ParseFactScope(p.Scope)
			if err := store.Set(scope, chatID, p.Key, p.Value, "memory_set"); err != nil {
				return ToolResult{IsError: true, Output: "保存失败：" + err.Error()}
			}
			return ToolResult{Output: fmt.Sprintf("已保存 [%s] %s: %s", scope, p.Key, p.Value)}
		},
	}
}

func memoryGetTool(store *FactStore, chatID string) Tool {
	return Tool{
		Name:        "memory_get",
		Description: "按 key 读取一个长期记忆事实。",
		Parameters: object(map[string]any{
			"key":   map[string]any{"type": "string", "description": "要读取的事实 key。"},
			"scope": map[string]any{"type": "string", "enum": []string{"chat", "project", "global"}, "description": "查找范围，默认 chat。"},
		}, "key"),
		Call: func(_ context.Context, args string) ToolResult {
			var p struct {
				Key   string `json:"key"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return ToolResult{IsError: true, Output: "参数解析失败：" + err.Error()}
			}
			scope := ParseFactScope(p.Scope)
			f, ok, err := store.Get(scope, chatID, p.Key)
			if err != nil {
				return ToolResult{IsError: true, Output: "读取失败：" + err.Error()}
			}
			if !ok {
				return ToolResult{Output: fmt.Sprintf("未找到 [%s] %s", scope, p.Key)}
			}
			return ToolResult{Output: fmt.Sprintf("[%s] %s: %s (更新于 %s)", scope, f.Key, f.Value, f.UpdatedAt.Format("2006-01-02 15:04"))}
		},
	}
}

func memoryListTool(store *FactStore, chatID string) Tool {
	return Tool{
		Name:        "memory_list",
		Description: "列出长期记忆事实。可指定 key 前缀过滤。",
		Parameters: object(map[string]any{
			"prefix": map[string]any{"type": "string", "description": "可选的 key 前缀，例如 user. 只显示用户相关事实。"},
			"scope":  map[string]any{"type": "string", "enum": []string{"chat", "project", "global"}, "description": "查找范围，默认 chat。"},
		}),
		Call: func(_ context.Context, args string) ToolResult {
			var p struct {
				Prefix string `json:"prefix"`
				Scope  string `json:"scope"`
			}
			if args != "" {
				if err := json.Unmarshal([]byte(args), &p); err != nil {
					return ToolResult{IsError: true, Output: "参数解析失败：" + err.Error()}
				}
			}
			scope := ParseFactScope(p.Scope)
			facts, err := store.List(scope, chatID, p.Prefix)
			if err != nil {
				return ToolResult{IsError: true, Output: "列出失败：" + err.Error()}
			}
			if len(facts) == 0 {
				return ToolResult{Output: fmt.Sprintf("[%s] 暂无记忆", scope)}
			}
			var sb strings.Builder
			fmt.Fprintf(&sb, "[%s] 共 %d 条记忆：\n", scope, len(facts))
			for _, f := range facts {
				fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
			}
			return ToolResult{Output: sb.String()}
		},
	}
}

func memoryDeleteTool(store *FactStore, chatID string) Tool {
	return Tool{
		Name:        "memory_delete",
		Description: "删除一个长期记忆事实。",
		Parameters: object(map[string]any{
			"key":   map[string]any{"type": "string", "description": "要删除的事实 key。"},
			"scope": map[string]any{"type": "string", "enum": []string{"chat", "project", "global"}, "description": "范围，默认 chat。"},
		}, "key"),
		Call: func(_ context.Context, args string) ToolResult {
			var p struct {
				Key   string `json:"key"`
				Scope string `json:"scope"`
			}
			if err := json.Unmarshal([]byte(args), &p); err != nil {
				return ToolResult{IsError: true, Output: "参数解析失败：" + err.Error()}
			}
			scope := ParseFactScope(p.Scope)
			if err := store.Delete(scope, chatID, p.Key); err != nil {
				return ToolResult{IsError: true, Output: "删除失败：" + err.Error()}
			}
			return ToolResult{Output: fmt.Sprintf("已删除 [%s] %s", scope, p.Key)}
		},
	}
}
