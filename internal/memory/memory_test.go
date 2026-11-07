package memory

import (
	"context"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/store"
	"companion/internal/testutil"
)

func setup(t *testing.T, emb *testutil.Embed, chat *testutil.Chat) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg, _ := config.Load(func(string) string { return "" })
	cfg.MinSim = 0.1
	return &Manager{St: st, Emb: emb, Maint: chat, MaintModel: "deepseek-x", Cfg: cfg, Now: time.Now}, st
}

func TestExtractStoresUserFactsWithModelTag(t *testing.T) {
	chat := &testutil.Chat{Replies: []string{
		`{"memories":[{"kind":"fact","text":"用户在学线性代数，周五有作业","importance":4}],"learning":[{"topic":"特征值","status":"learning","error_pattern":"忘记检查特征向量非零"}]}`,
	}}
	m, st := setup(t, &testutil.Embed{Name: "e1"}, chat)
	st.AddMessage(1, "user", "我在学线性代数，周五有作业")
	st.AddMessage(1, "assistant", "好呀，我陪你")
	n, err := m.Extract(context.Background(), 1)
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	mems := st.ActiveMemories()
	if len(mems) != 1 || mems[0].ByModel != "deepseek-x" || mems[0].EmbedModel != "e1" || len(mems[0].Emb) == 0 {
		t.Fatalf("%+v", mems)
	}
	if len(st.ListLearning(5)) != 1 || st.ExtractedUpTo(1) == 0 {
		t.Fatal("learning / cursor not stored")
	}
}

func TestBadJSONDoesNotAdvanceCursor(t *testing.T) {
	chat := &testutil.Chat{Replies: []string{"抱歉我不会"}}
	m, st := setup(t, &testutil.Embed{Name: "e1"}, chat)
	st.AddMessage(1, "user", "hello there friend")
	m.Extract(context.Background(), 1)
	if st.ExtractedUpTo(1) != 0 {
		t.Fatal("cursor advanced on unparseable output; messages would be lost")
	}
}

// The user's core requirement: memory built with one model keeps working after swapping models.
func TestModelSwapKeepsMemoriesUsable(t *testing.T) {
	ctx := context.Background()
	m, st := setup(t, &testutil.Embed{Name: "old"}, &testutil.Chat{})
	for _, txt := range []string{"用户在学线性代数", "用户喜欢喝乌龙茶", "用户养了一只猫叫团子"} {
		v, _ := m.Emb.Embed(ctx, []string{txt})
		st.AddMemory(store.Memory{Kind: "fact", Text: txt, Importance: 3, Status: "active", Created: float64(time.Now().Unix()),
			Emb: v[0], EmbedModel: "old", ByModel: "deepseek-v4"})
	}
	// swap the embedding model
	m.Emb = &testutil.Embed{Name: "new", Dim: 96}
	if m.NeedReembed() != 3 {
		t.Fatalf("NeedReembed=%d", m.NeedReembed())
	}
	// before re-embedding: no crash, keyword fallback still finds the memory
	got, err := m.Retrieve(ctx, "线性代数", 3)
	if err != nil || len(got) == 0 || got[0].Text != "用户在学线性代数" {
		t.Fatalf("keyword fallback failed: %v %v", got, err)
	}
	n, err := m.Reembed(ctx)
	if err != nil || n != 3 || m.NeedReembed() != 0 {
		t.Fatalf("reembed n=%d err=%v need=%d", n, err, m.NeedReembed())
	}
	got, _ = m.Retrieve(ctx, "我的猫团子", 1)
	if len(got) != 1 || got[0].Text != "用户养了一只猫叫团子" {
		t.Fatalf("vector retrieval after reembed: %v", got)
	}
	if got[0].ByModel != "deepseek-v4" {
		t.Fatal("provenance must survive re-embedding")
	}
}
