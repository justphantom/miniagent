package miniagent

import (
	"context"
	"strings"
	"sync"
	"testing"
)

func TestTodo_AddListComplete(t *testing.T) {
	tl := &TaskList{}
	tool := TodoTool(tl)
	r1 := tool.Call(context.Background(), `{"action":"add","subject":"第一项"}`)
	if r1.IsError {
		t.Fatalf("add: %s", r1.Output)
	}
	if !strings.Contains(r1.Output, "id=1") {
		t.Errorf("first id should be 1: %s", r1.Output)
	}
	r2 := tool.Call(context.Background(), `{"action":"add","subject":"第二项"}`)
	if !strings.Contains(r2.Output, "id=2") {
		t.Errorf("second id should be 2: %s", r2.Output)
	}
	rL := tool.Call(context.Background(), `{"action":"list"}`)
	if !strings.Contains(rL.Output, "第一项") || !strings.Contains(rL.Output, "第二项") {
		t.Errorf("list missing items: %s", rL.Output)
	}
	rc := tool.Call(context.Background(), `{"action":"complete","id":1}`)
	if rc.IsError {
		t.Fatalf("complete: %s", rc.Output)
	}
	if rL2 := tool.Call(context.Background(), `{"action":"list"}`); !strings.Contains(rL2.Output, "completed") {
		t.Errorf("not marked completed: %s", rL2.Output)
	}
}

func TestTodo_InvalidStatusRejected(t *testing.T) {
	tl := &TaskList{}
	tool := TodoTool(tl)
	tool.Call(context.Background(), `{"action":"add","subject":"x"}`)
	r := tool.Call(context.Background(), `{"action":"update","id":1,"status":"bogus"}`)
	if !r.IsError {
		t.Errorf("invalid status should fail: %s", r.Output)
	}
}

func TestTodo_UnknownID(t *testing.T) {
	tl := &TaskList{}
	tool := TodoTool(tl)
	if r := tool.Call(context.Background(), `{"action":"complete","id":99}`); !r.IsError {
		t.Errorf("unknown id should fail: %s", r.Output)
	}
}

// 并发 add：id 无重复、无数据竞争（go test -race 验证）。
func TestTodo_ConcurrentAdd(t *testing.T) {
	tl := &TaskList{}
	tool := TodoTool(tl)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tool.Call(context.Background(), `{"action":"add","subject":"t"}`)
		}()
	}
	wg.Wait()
	r := tool.Call(context.Background(), `{"action":"list"}`)
	if got := len(strings.Split(strings.TrimSpace(r.Output), "\n")); got != 20 {
		t.Errorf("got %d items, want 20", got)
	}
}
