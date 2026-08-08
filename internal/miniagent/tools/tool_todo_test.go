package tools

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

func TestTodoList_CreatePending(t *testing.T) {
	l := &TodoList{}
	it := l.Create("写测试", "覆盖 CRUD")
	if it.ID != 1 || it.Subject != "写测试" || it.Description != "覆盖 CRUD" || it.Status != TodoPending {
		t.Errorf("create = %+v", it)
	}
	it2 := l.Create("第二个", "")
	if it2.ID != 2 {
		t.Errorf("id 应自增，got %d", it2.ID)
	}
}

func TestTodoList_UpdateFields(t *testing.T) {
	l := &TodoList{}
	l.Create("任务", "")
	it, ok := l.Update(1, TodoInProgress, "", "")
	if !ok || it.Status != TodoInProgress {
		t.Errorf("update status = %+v ok=%v", it, ok)
	}
	it, ok = l.Update(1, "", "新标题", "新详情")
	if !ok || it.Subject != "新标题" || it.Description != "新详情" || it.Status != TodoInProgress {
		t.Errorf("update fields = %+v", it)
	}
	// 空字段不应清空已有值
	it, ok = l.Update(1, "", "", "")
	if !ok || it.Subject != "新标题" || it.Status != TodoInProgress {
		t.Errorf("empty fields should not clear: %+v", it)
	}
	if _, ok := l.Update(99, TodoCompleted, "", ""); ok {
		t.Error("update 不存在的 id 应 ok=false")
	}
}

func TestTodoList_ListSorted(t *testing.T) {
	l := &TodoList{}
	l.Create("b", "")
	l.Create("a", "")
	items := l.List()
	if len(items) != 2 || items[0].ID != 1 || items[1].ID != 2 {
		t.Errorf("list = %+v", items)
	}
	if len((&TodoList{}).List()) != 0 {
		t.Error("空 list 应返回空切片")
	}
}

func TestTodoTools_CreateValidation(t *testing.T) {
	l := &TodoList{}
	create := TodoTools(l)[0]
	if r := create.Call(context.Background(), `{"description":"无 subject"}`); !r.IsError {
		t.Errorf("缺 subject 应 IsError: %s", r.Output)
	}
	r := create.Call(context.Background(), `{"subject":"任务一"}`)
	if r.IsError {
		t.Fatalf("正常创建应成功: %s", r.Output)
	}
	var it TodoItem
	if err := json.Unmarshal([]byte(r.Output), &it); err != nil || it.Subject != "任务一" || it.Status != TodoPending || it.ID != 1 {
		t.Errorf("create 输出 = %s", r.Output)
	}
}

func TestTodoTools_UpdateValidation(t *testing.T) {
	l := &TodoList{}
	create, update := TodoTools(l)[0], TodoTools(l)[1]
	create.Call(context.Background(), `{"subject":"任务"}`)
	if r := update.Call(context.Background(), `{"status":"completed"}`); !r.IsError {
		t.Errorf("缺 id 应 IsError: %s", r.Output)
	}
	if r := update.Call(context.Background(), `{"id":1,"status":"done"}`); !r.IsError {
		t.Errorf("非法 status 应 IsError: %s", r.Output)
	}
	if r := update.Call(context.Background(), `{"id":99,"status":"completed"}`); !r.IsError {
		t.Errorf("id 不存在应 IsError: %s", r.Output)
	}
	r := update.Call(context.Background(), `{"id":1,"status":"completed"}`)
	if r.IsError || !strings.Contains(r.Output, `"status":"completed"`) {
		t.Errorf("正常更新应成功: %s", r.Output)
	}
}

func TestTodoTools_List(t *testing.T) {
	l := &TodoList{}
	list := TodoTools(l)[2]
	if r := list.Call(context.Background(), `{}`); r.IsError || !strings.Contains(r.Output, "无任务") {
		t.Errorf("空 list: %s", r.Output)
	}
	l.Create("任务A", "详情")
	l.Create("任务B", "")
	r := list.Call(context.Background(), `{}`)
	if r.IsError || !strings.Contains(r.Output, "#1 [pending] 任务A") || !strings.Contains(r.Output, "详情") || !strings.Contains(r.Output, "#2 [pending] 任务B") {
		t.Errorf("list 输出: %s", r.Output)
	}
}

// 并发：runToolsParallel 并行多 create 不竞争（-race 验证）、id 无重复。
func TestTodoList_ConcurrentCreate(t *testing.T) {
	l := &TodoList{}
	const n = 100
	var wg sync.WaitGroup
	ids := make(chan int, n)
	for range n {
		wg.Go(func() {
			ids <- l.Create("并发", "").ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int]bool{}
	count := 0
	for id := range ids {
		count++
		if seen[id] {
			t.Errorf("id %d 重复", id)
		}
		seen[id] = true
	}
	if count != n {
		t.Errorf("并发 create 数 = %d, want %d", count, n)
	}
}
