package bot

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"companion/internal/config"
	"companion/internal/llm"
	"companion/internal/store"
	"companion/internal/testutil"
)

func newReviewApp(t *testing.T, env map[string]string, reviewReplies ...string) (*App, *fakeTG, *testutil.Chat, *testutil.Chat, *store.Store, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	base := map[string]string{"DATA_DIR": dir, "ALLOWED_USER_IDS": "42", "LLM_MODEL": "chat-m", "EXTRACT_EVERY": "100"}
	for k, v := range env {
		base[k] = v
	}
	cfg, err := config.Load(func(k string) string { return base[k] })
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	now := time.Now()
	clock := &now
	tg := &fakeTG{}
	chat := &testutil.Chat{Replies: []string{"好的呀"}}
	rev := &testutil.Chat{Replies: reviewReplies}
	maint := &testutil.Chat{Replies: []string{`{"memories":[],"learning":[]}`}}
	app, err := New(cfg, Deps{Store: st, Chat: chat, Maint: maint, Review: rev, Embed: &testutil.Embed{Name: "e"}, TG: tg,
		Now: func() time.Time { return *clock }})
	if err != nil {
		t.Fatal(err)
	}
	return app, tg, chat, rev, st, clock
}

func addTalk(t *testing.T, st *store.Store, chat int64, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		if _, err := st.AddMessage(chat, role, fmt.Sprintf("消息%d", i)); err != nil {
			t.Fatal(err)
		}
	}
}

const proposal = `{"skip":false,"notes":"- 回复不超过三句","changes":["+ 回复不超过三句"],"evidence":"他说“太长了”"}`

func TestReviewProposeApproveAndInject(t *testing.T) {
	app, tg, chat, rev, st, _ := newReviewApp(t, nil, proposal)
	ctx := context.Background()
	addTalk(t, st, 42, 4)

	app.command(ctx, 42, "/style review")
	if !strings.Contains(tg.last(), "/style approve") || !strings.Contains(tg.last(), "+ 回复不超过三句") {
		t.Fatalf("proposal message: %q", tg.last())
	}
	in := fmt.Sprint(rev.Calls[0][1].Content)
	if !strings.Contains(in, "【最近的对话】") || !strings.Contains(in, "他：消息0") || !strings.Contains(in, "她：消息1") {
		t.Fatalf("review input missing conversation:\n%s", in)
	}
	if app.style.Text() != "" {
		t.Fatal("a proposal must not take effect before approval")
	}
	app.command(ctx, 42, "/style approve")
	if app.style.Text() != "- 回复不超过三句" {
		t.Fatalf("approved text %q", app.style.Text())
	}

	// the notes reach the chat model's system prompt
	if _, err := app.Respond(ctx, 42, "在吗", nil); err != nil {
		t.Fatal(err)
	}
	sys := fmt.Sprint(chat.Calls[len(chat.Calls)-1][0].Content)
	if !strings.Contains(sys, "## 说话风格笔记") || !strings.Contains(sys, "- 回复不超过三句") {
		t.Fatalf("style notes not injected:\n%s", sys)
	}

	// already reviewed messages are not reviewed again (only the new turn is)
	app.command(ctx, 42, "/style review")
	in2 := fmt.Sprint(rev.Calls[1][1].Content)
	if strings.Contains(in2, "消息0") || !strings.Contains(in2, "在吗") {
		t.Fatalf("second review should only see new messages:\n%s", in2)
	}
}

func TestReviewRejectsOversizeAndKeepsCursor(t *testing.T) {
	long := `{"skip":false,"notes":"` + strings.Repeat("长", 50) + `","changes":[]}`
	app, tg, _, rev, st, _ := newReviewApp(t, map[string]string{"STYLE_MAX_CHARS": "20"}, long)
	addTalk(t, st, 42, 2)
	app.command(context.Background(), 42, "/style review")
	if !strings.Contains(tg.last(), "超过上限") {
		t.Fatalf("expected oversize error, got %q", tg.last())
	}
	if app.reviewCursor(42) != 0 {
		t.Fatal("a failed review must not mark messages as reviewed")
	}
	if app.style.Pending() != nil || len(rev.Calls) != 1 {
		t.Fatal("no proposal on failure")
	}
}

func TestReviewBadFeedbackAndReject(t *testing.T) {
	app, tg, _, rev, st, _ := newReviewApp(t, nil, proposal)
	ctx := context.Background()
	addTalk(t, st, 42, 2) // last message is the assistant's "消息1"
	app.command(ctx, 42, "/style bad 太像客服")
	if len(app.style.Feedback()) != 1 {
		t.Fatal("feedback not stored")
	}
	app.command(ctx, 42, "/style review")
	in := fmt.Sprint(rev.Calls[0][1].Content)
	if !strings.Contains(in, "太像客服") || !strings.Contains(in, "回复：消息1") {
		t.Fatalf("feedback not passed to reviewer:\n%s", in)
	}
	if len(app.style.Feedback()) != 0 {
		t.Fatal("feedback should be consumed by the review")
	}
	app.command(ctx, 42, "/style reject")
	if app.style.Pending() != nil || !strings.Contains(tg.last(), "已丢弃") {
		t.Fatalf("reject: %q", tg.last())
	}
	addTalk(t, st, 42, 2)
	app.command(ctx, 42, "/style review")
	if in := fmt.Sprint(rev.Calls[1][1].Content); !strings.Contains(in, "【他拒绝过的改动】") {
		t.Fatalf("rejected changes not shown to the reviewer:\n%s", in)
	}
}

func TestReviewSkip(t *testing.T) {
	app, tg, _, _, st, _ := newReviewApp(t, nil, `{"skip":true,"reason":"没有新的反馈"}`)
	addTalk(t, st, 42, 2)
	app.command(context.Background(), 42, "/style review")
	if !strings.Contains(tg.last(), "不需要改") || app.reviewCursor(42) == 0 {
		t.Fatalf("skip: %q cursor=%d", tg.last(), app.reviewCursor(42))
	}
	app.command(context.Background(), 42, "/style review")
	if !strings.Contains(tg.last(), "没有新的对话") {
		t.Fatalf("nothing to review: %q", tg.last())
	}
}

func TestMaybeReviewScheduling(t *testing.T) {
	app, tg, _, rev, st, clock := newReviewApp(t, map[string]string{"REVIEW_MIN_MESSAGES": "4", "IDLE_MINUTES": "20"}, proposal)
	ctx := context.Background()
	addTalk(t, st, 42, 2)
	*clock = clock.Add(time.Hour)
	app.MaybeReview(ctx)
	app.Wait()
	if len(rev.Calls) != 0 {
		t.Fatal("too few messages: no review")
	}
	addTalk(t, st, 42, 2) // messages are stamped with real time.Now
	*clock = time.Now()   // the user is still talking
	app.MaybeReview(ctx)
	app.Wait()
	if len(rev.Calls) != 0 {
		t.Fatal("not idle yet: no review")
	}
	*clock = time.Now().Add(time.Hour)
	app.MaybeReview(ctx)
	app.Wait()
	if len(rev.Calls) != 1 || !strings.Contains(tg.last(), "/style approve") {
		t.Fatalf("expected one automatic review, calls=%d last=%q", len(rev.Calls), tg.last())
	}
	addTalk(t, st, 42, 6)
	*clock = clock.Add(2 * time.Hour)
	app.MaybeReview(ctx)
	app.Wait()
	if len(rev.Calls) != 1 {
		t.Fatal("REVIEW_HOURS not respected")
	}
	app2, _, _, rev2, st2, clock2 := newReviewApp(t, map[string]string{"REVIEW_HOURS": "0", "REVIEW_MIN_MESSAGES": "1"}, proposal)
	addTalk(t, st2, 42, 4)
	*clock2 = clock2.Add(time.Hour)
	app2.MaybeReview(ctx)
	app2.Wait()
	if len(rev2.Calls) != 0 {
		t.Fatal("REVIEW_HOURS=0 must disable automatic reviews")
	}
}

func TestReviewAutoApply(t *testing.T) {
	app, tg, _, _, st, _ := newReviewApp(t, map[string]string{"REVIEW_AUTO_APPLY": "true"}, proposal)
	addTalk(t, st, 42, 2)
	app.command(context.Background(), 42, "/style review")
	if app.style.Text() != "- 回复不超过三句" || !strings.Contains(tg.last(), "已自动更新到 v1") {
		t.Fatalf("auto apply: %q / %q", app.style.Text(), tg.last())
	}
	app.command(context.Background(), 42, "/style rollback")
	if app.style.Text() != "" {
		t.Fatal("rollback after auto apply")
	}
}

func TestReviewConfigFallbackAndMaxTokens(t *testing.T) {
	cfg, err := config.Load(func(k string) string {
		return map[string]string{"LLM_BASE_URL": "https://ds", "LLM_MODEL": "chat", "MAINT_MODEL": "cheap"}[k]
	})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Review.Model != "cheap" || cfg.Review.BaseURL != "https://ds" || cfg.Review.ExtraBody["max_tokens"] != 8000 {
		t.Fatalf("review endpoint fallback: %+v", cfg.Review)
	}
	if cfg.Maint.ExtraBody != nil {
		t.Fatal("max_tokens default must not leak into MAINT")
	}
	cfg, _ = config.Load(func(k string) string {
		return map[string]string{"LLM_MODEL": "chat", "REVIEW_BASE_URL": "https://api.anthropic.com/v1",
			"REVIEW_MODEL": "claude-opus-5-5", "REVIEW_EXTRA_BODY": `{"max_tokens":3000}`}[k]
	})
	if cfg.Review.Model != "claude-opus-5-5" || cfg.Review.ExtraBody["max_tokens"] != float64(3000) {
		t.Fatalf("explicit review endpoint: %+v", cfg.Review)
	}
	var _ llm.Completer = (*testutil.Chat)(nil)
}

func TestReviewCostShownOnlyForSameProvider(t *testing.T) {
	app, _, _, _, _, _ := newReviewApp(t, map[string]string{"LLM_BASE_URL": "https://ds", "PRICE_OUTPUT": "8"})
	app.tr.hook("review")(llm.Usage{Out: 1000000})
	if s := app.describeUsage("review", "风格复盘"); !strings.Contains(s, "约 8.00") {
		t.Fatalf("same provider should be priced: %q", s)
	}
	app2, _, _, _, _, _ := newReviewApp(t, map[string]string{"LLM_BASE_URL": "https://ds", "REVIEW_BASE_URL": "https://other", "PRICE_OUTPUT": "8"})
	app2.tr.hook("review")(llm.Usage{Out: 1000000})
	if s := app2.describeUsage("review", "风格复盘"); strings.Contains(s, "约") {
		t.Fatalf("other provider must not be priced: %q", s)
	}
}
