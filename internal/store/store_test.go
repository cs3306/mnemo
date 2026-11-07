package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplayAndCompaction(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := s.AddMemory(Memory{Kind: "fact", Text: "喜欢线性代数", Importance: 3, Status: "active", Emb: Vec{1, 0}})
	id2, _ := s.AddMemory(Memory{Kind: "fact", Text: "喜欢微积分", Importance: 3, Status: "active"})
	s.Supersede(id1, id2)
	s.AddMessage(7, "user", "hi")
	s.SetCore("core", "t")
	s.AddPin("pin1", 5)
	s.UpsertLearning("极限", "learning", "ε-δ 弄混")
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	s.AddMessage(7, "assistant", "hello")
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	m, _ := s2.GetMemory(id1)
	if m.Status != "superseded" || m.SupersededBy != id2 || len(m.Emb) != 2 {
		t.Fatalf("replay lost state: %+v", m)
	}
	if len(s2.ActiveMemories()) != 1 || s2.Core() != "core" || len(s2.Pins()) != 1 || len(s2.ListLearning(5)) != 1 {
		t.Fatal("state not restored")
	}
	if len(s2.Unsummarized(7)) != 2 {
		t.Fatal("messages not restored")
	}
}

func TestDeleteIsHardAndExportImport(t *testing.T) {
	dir := t.TempDir()
	s, _ := Open(dir)
	id, _ := s.AddMemory(Memory{Kind: "fact", Text: "秘密内容XYZ", Importance: 2, Status: "active", ByModel: "deepseek"})
	s.AddMemory(Memory{Kind: "pref", Text: "喜欢简短回答", Importance: 4, Status: "active", Emb: Vec{1}, EmbedModel: "a", ByModel: "deepseek"})
	ex := s.Export()
	if len(ex.Memories) != 2 || ex.Memories[1].ByModel != "deepseek" {
		t.Fatalf("export: %+v", ex)
	}
	if ok, err := s.DeleteMemory(id); !ok || err != nil {
		t.Fatal(ok, err)
	}
	s.Close()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
		if e.Type().IsRegular() && contains(string(b), "秘密内容XYZ") {
			t.Fatalf("deleted memory still on disk in %s", e.Name())
		}
	}
	s3, _ := Open(t.TempDir())
	defer s3.Close()
	n, err := s3.Import(ex)
	if err != nil || n != 2 {
		t.Fatal(n, err)
	}
	for _, m := range s3.ActiveMemories() {
		if len(m.Emb) != 0 {
			t.Fatal("import must not carry vectors")
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
