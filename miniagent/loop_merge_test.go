package miniagent

import "testing"

// mergePersisted is the production path of compaction persistence (loop.go): persisted entries carrying a
// Kind replace same-Kind old entries in newMsgs, then are prepended to the head of newMsgs — multiple
// compactions keep only the latest summary, and that summary sits before the user_prompt (the cross-turn
// barrier hits the newest). This path had zero direct tests for a long time (only indirectly covered by
// SilentOverflow, which only checks the res.Compacted bool); this test pins Kind dedup + prepend order.
func TestMergePersisted_ReplaceByKindAndPrepend(t *testing.T) {
	// Scenario 1: the new summary replaces the old one and is prepended before the user.
	newMsgs := []Message{
		{Role: RoleUser, Content: "old question"},
		{Kind: KindSummary, Content: "old summary"},
	}
	mergePersisted(&newMsgs, []Message{{Kind: KindSummary, Content: "new summary"}})
	if len(newMsgs) != 2 {
		t.Fatalf("len=%d, want 2 (old summary replaced, new summary prepended): %+v", len(newMsgs), newMsgs)
	}
	if newMsgs[0].Kind != KindSummary || newMsgs[0].Content != "new summary" {
		t.Errorf("newMsgs[0] should be the new summary, got %+v", newMsgs[0])
	}
	if newMsgs[1].Role != RoleUser || newMsgs[1].Content != "old question" {
		t.Errorf("newMsgs[1] should be the old user (preserved by the prepend), got %+v", newMsgs[1])
	}

	// Scenario 2: multiple Kinds — different Kinds dedup independently; Kinds not present in persisted stay in place.
	newMsgs2 := []Message{
		{Kind: "k1", Content: "old1"},
		{Kind: "k2", Content: "old2"},
		{Role: RoleUser, Content: "u"},
	}
	mergePersisted(&newMsgs2, []Message{{Kind: "k1", Content: "new1"}})
	if len(newMsgs2) != 3 {
		t.Fatalf("len=%d, want 3 (k1 replaced, k2 and u preserved): %+v", len(newMsgs2), newMsgs2)
	}
	if newMsgs2[0].Kind != "k1" || newMsgs2[0].Content != "new1" {
		t.Errorf("[0] want new1(k1), got %+v", newMsgs2[0])
	}
	if newMsgs2[1].Kind != "k2" || newMsgs2[1].Content != "old2" {
		t.Errorf("[1] want old2(k2) preserved, got %+v", newMsgs2[1])
	}
	if newMsgs2[2].Role != RoleUser || newMsgs2[2].Content != "u" {
		t.Errorf("[2] want u(user) preserved, got %+v", newMsgs2[2])
	}

	// Scenario 3: persisted without a Kind (e.g. an ordinary injected message) — no dedup, prepended as-is.
	newMsgs3 := []Message{{Role: RoleUser, Content: "q"}}
	mergePersisted(&newMsgs3, []Message{{Role: RoleSystem, Content: "sys"}})
	if len(newMsgs3) != 2 || newMsgs3[0].Role != RoleSystem || newMsgs3[1].Content != "q" {
		t.Errorf("persisted without a Kind should be prepended as-is: got %+v", newMsgs3)
	}

	// Scenario 4: empty persisted leaves newMsgs unchanged.
	newMsgs4 := []Message{{Role: RoleUser, Content: "q"}}
	mergePersisted(&newMsgs4, nil)
	if len(newMsgs4) != 1 || newMsgs4[0].Content != "q" {
		t.Errorf("empty persisted should leave newMsgs unchanged: got %+v", newMsgs4)
	}
}
