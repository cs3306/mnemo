package style

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVersioningApproveRejectRollback(t *testing.T) {
	p := filepath.Join(t.TempDir(), "style.json")
	s, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Approve(); !errors.Is(err, ErrNoPending) {
		t.Fatalf("approve without proposal: %v", err)
	}
	if err := s.Propose(Proposal{Text: "- 短句", Changes: []string{"+ 短句"}}); err != nil {
		t.Fatal(err)
	}
	v, err := s.Approve()
	if err != nil || v.N != 1 || v.Text != "- 短句" {
		t.Fatalf("approve: %+v %v", v, err)
	}
	if err := s.Propose(Proposal{Text: "- 撒娇", Changes: []string{"+ 撒娇"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Reject(); err != nil {
		t.Fatal(err)
	}
	if r := s.Rejected(); len(r) != 1 || r[0][0] != "+ 撒娇" {
		t.Fatalf("rejected: %v", r)
	}
	if _, err := s.Set("- 手改", "manual"); err != nil {
		t.Fatal(err)
	}
	// proposal computed against v2 becomes stale once the notes change
	_ = s.Propose(Proposal{Text: "x"})
	_, _ = s.Set("- 又改", "manual")
	if s.Pending() != nil {
		t.Fatal("Set must drop the pending proposal")
	}
	_ = s.Propose(Proposal{Text: "y"})
	s.d.Active.N = 99 // simulate a concurrent change
	if _, err := s.Approve(); !errors.Is(err, ErrStale) {
		t.Fatalf("stale approve: %v", err)
	}
	s.d.Active.N = 3

	// reopen: everything persisted
	s2, err := Open(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Text() != "- 又改" {
		t.Fatalf("persisted text %q", s2.Text())
	}
	v, err = s2.Rollback()
	if err != nil || v.Text != "- 手改" {
		t.Fatalf("rollback: %+v %v", v, err)
	}
	v, _ = s2.Rollback()
	if v.Text != "- 短句" {
		t.Fatalf("second rollback: %+v", v)
	}
	// new versions keep increasing numbers after a rollback
	v, _ = s2.Set("- 新", "manual")
	if v.N != 4 {
		t.Fatalf("version after rollback = %d, want 4", v.N)
	}
}

func TestFeedbackConsume(t *testing.T) {
	s, _ := Open(filepath.Join(t.TempDir(), "style.json"), nil)
	_, _ = s.AddFeedback("太官方了", "像客服")
	fb := s.Feedback()
	_, _ = s.AddFeedback("后来的", "")
	s.d.Feedback[1].TS = fb[0].TS + 1
	if err := s.ConsumeFeedback(fb[0].TS); err != nil {
		t.Fatal(err)
	}
	if left := s.Feedback(); len(left) != 1 || left[0].Reply != "后来的" {
		t.Fatalf("left: %+v", left)
	}
}

func TestCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "style.json")
	_ = os.WriteFile(p, []byte("{not json"), 0o600)
	s, err := Open(p, nil)
	if err != nil || s.Text() != "" {
		t.Fatalf("open corrupt: %v", err)
	}
	m, _ := filepath.Glob(p + ".corrupt-*")
	if len(m) != 1 {
		t.Fatalf("corrupt file not kept: %v", m)
	}
}
