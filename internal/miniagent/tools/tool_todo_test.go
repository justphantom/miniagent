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
	it := l.Create("write tests", "cover CRUD")
	if it.ID != 1 || it.Subject != "write tests" || it.Description != "cover CRUD" || it.Status != TodoPending {
		t.Errorf("create = %+v", it)
	}
	it2 := l.Create("second", "")
	if it2.ID != 2 {
		t.Errorf("id should auto-increment, got %d", it2.ID)
	}
}

func TestTodoList_UpdateFields(t *testing.T) {
	l := &TodoList{}
	l.Create("task", "")
	it, ok := l.Update(1, TodoInProgress, "", "")
	if !ok || it.Status != TodoInProgress {
		t.Errorf("update status = %+v ok=%v", it, ok)
	}
	it, ok = l.Update(1, "", "new title", "new details")
	if !ok || it.Subject != "new title" || it.Description != "new details" || it.Status != TodoInProgress {
		t.Errorf("update fields = %+v", it)
	}
	// empty fields should not clear existing values
	it, ok = l.Update(1, "", "", "")
	if !ok || it.Subject != "new title" || it.Status != TodoInProgress {
		t.Errorf("empty fields should not clear: %+v", it)
	}
	if _, ok := l.Update(99, TodoCompleted, "", ""); ok {
		t.Error("update on non-existent id should return ok=false")
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
		t.Error("empty list should return empty slice")
	}
}

func TestTodoTools_CreateValidation(t *testing.T) {
	l := &TodoList{}
	create := TodoTools(l)[0]
	if r := create.Call(context.Background(), `{"description":"no subject"}`); !r.IsError {
		t.Errorf("missing subject should IsError: %s", r.Output)
	}
	r := create.Call(context.Background(), `{"subject":"task one"}`)
	if r.IsError {
		t.Fatalf("normal create should succeed: %s", r.Output)
	}
	var it TodoItem
	if err := json.Unmarshal([]byte(r.Output), &it); err != nil || it.Subject != "task one" || it.Status != TodoPending || it.ID != 1 {
		t.Errorf("create output = %s", r.Output)
	}
}

func TestTodoTools_UpdateValidation(t *testing.T) {
	l := &TodoList{}
	create, update := TodoTools(l)[0], TodoTools(l)[1]
	create.Call(context.Background(), `{"subject":"task"}`)
	if r := update.Call(context.Background(), `{"status":"completed"}`); !r.IsError {
		t.Errorf("missing id should IsError: %s", r.Output)
	}
	if r := update.Call(context.Background(), `{"id":1,"status":"done"}`); !r.IsError {
		t.Errorf("invalid status should IsError: %s", r.Output)
	}
	if r := update.Call(context.Background(), `{"id":99,"status":"completed"}`); !r.IsError {
		t.Errorf("non-existent id should IsError: %s", r.Output)
	}
	r := update.Call(context.Background(), `{"id":1,"status":"completed"}`)
	if r.IsError || !strings.Contains(r.Output, `"status":"completed"`) {
		t.Errorf("normal update should succeed: %s", r.Output)
	}
}

func TestTodoTools_List(t *testing.T) {
	l := &TodoList{}
	list := TodoTools(l)[2]
	if r := list.Call(context.Background(), `{}`); r.IsError || !strings.Contains(r.Output, "no tasks") {
		t.Errorf("empty list: %s", r.Output)
	}
	l.Create("Task A", "details")
	l.Create("Task B", "")
	r := list.Call(context.Background(), `{}`)
	if r.IsError || !strings.Contains(r.Output, "#1 [pending] Task A") || !strings.Contains(r.Output, "details") || !strings.Contains(r.Output, "#2 [pending] Task B") {
		t.Errorf("list output: %s", r.Output)
	}
}

// Concurrency: runToolsParallel runs multiple creates in parallel without contention (verified with -race), ids are unique.
func TestTodoList_ConcurrentCreate(t *testing.T) {
	l := &TodoList{}
	const n = 100
	var wg sync.WaitGroup
	ids := make(chan int, n)
	for range n {
		wg.Go(func() {
			ids <- l.Create("concurrent", "").ID
		})
	}
	wg.Wait()
	close(ids)
	seen := map[int]bool{}
	count := 0
	for id := range ids {
		count++
		if seen[id] {
			t.Errorf("id %d duplicated", id)
		}
		seen[id] = true
	}
	if count != n {
		t.Errorf("concurrent create count = %d, want %d", count, n)
	}
}
