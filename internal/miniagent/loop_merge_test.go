package miniagent

import "testing"

// mergePersisted 是压缩持久化的生产路径（loop.go）：带 Kind 的 persisted 替换 newMsgs 中同 Kind 旧条目，
// 再前插到 newMsgs 首部——多次压缩只留最新 summary，且 summary 排在 user_prompt 之前（跨轮 barrier 命中最新）。
// 该路径长期零直测（仅 SilentOverflow 间接覆盖且只查 res.Compacted bool），本测试锁定 Kind 去重 + 前插顺序。
func TestMergePersisted_ReplaceByKindAndPrepend(t *testing.T) {
	// 场景1：新 summary 替换旧 summary，前插到 user 之前。
	newMsgs := []Message{
		{Role: RoleUser, Content: "旧问题"},
		{Kind: KindSummary, Content: "旧摘要"},
	}
	mergePersisted(&newMsgs, []Message{{Kind: KindSummary, Content: "新摘要"}})
	if len(newMsgs) != 2 {
		t.Fatalf("len=%d, want 2（旧 summary 被替换、新 summary 前插）: %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != KindSummary || newMsgs[0].Content != "新摘要" {
		t.Errorf("newMsgs[0] 应为新 summary，got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != RoleUser || newMsgs[1].Content != "旧问题" {
		t.Errorf("newMsgs[1] 应为旧 user（前插保留），got %+v", newMsgs[1])
	}

	// 场景2：多 Kind——不同 Kind 各自去重，未在 persisted 中的 Kind 保留原位。
	newMsgs2 := []Message{
		{Kind: "k1", Content: "old1"},
		{Kind: "k2", Content: "old2"},
		{Role: RoleUser, Content: "u"},
	}
	mergePersisted(&newMsgs2, []Message{{Kind: "k1", Content: "new1"}})
	if len(newMsgs2) != 3 {
		t.Fatalf("len=%d, want 3（k1 替换、k2 与 u 保留）: %+v", len(newMsgs2), newMsgs2)
	}
	if newMsgs2[0].Kind != "k1" || newMsgs2[0].Content != "new1" {
		t.Errorf("[0] want new1(k1)，got %+v", newMsgs2[0])
	}
	if newMsgs2[1].Kind != "k2" || newMsgs2[1].Content != "old2" {
		t.Errorf("[1] want old2(k2) 保留，got %+v", newMsgs2[1])
	}
	if newMsgs2[2].Role != RoleUser || newMsgs2[2].Content != "u" {
		t.Errorf("[2] want u(user) 保留，got %+v", newMsgs2[2])
	}

	// 场景3：persisted 无 Kind（如普通注入消息）——不触发去重，原样前插。
	newMsgs3 := []Message{{Role: RoleUser, Content: "q"}}
	mergePersisted(&newMsgs3, []Message{{Role: RoleSystem, Content: "sys"}})
	if len(newMsgs3) != 2 || newMsgs3[0].Role != RoleSystem || newMsgs3[1].Content != "q" {
		t.Errorf("无 Kind 的 persisted 应原样前插：got %+v", newMsgs3)
	}

	// 场景4：空 persisted 不改 newMsgs。
	newMsgs4 := []Message{{Role: RoleUser, Content: "q"}}
	mergePersisted(&newMsgs4, nil)
	if len(newMsgs4) != 1 || newMsgs4[0].Content != "q" {
		t.Errorf("空 persisted 应不改 newMsgs：got %+v", newMsgs4)
	}
}
