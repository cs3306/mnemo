// Package style keeps the learned "speaking style notes": a short, versioned block of rules injected after
// persona.md. The notes are written by the review job (see bot/review.go) or by hand; a review never changes
// them directly unless REVIEW_AUTO_APPLY is set — it leaves a pending proposal that the user approves or rejects.
//
// Everything lives in one small JSON file (style.json in the data directory), written atomically.
package style

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	maxHistory  = 10 // previous versions kept for /style rollback
	maxRejected = 5  // recently rejected change lists, shown to the reviewer so it stops re-proposing them
	maxFeedback = 20 // /style bad entries waiting for the next review
)

// Version is one state of the notes. N=0 with empty Text is "no notes yet".
type Version struct {
	N      int     `json:"n"`
	Text   string  `json:"text"`
	TS     float64 `json:"ts"`
	Reason string  `json:"reason,omitempty"`
}

// Proposal is a review result waiting for /style approve. Base is the version it was computed against.
type Proposal struct {
	Text     string   `json:"text"`
	Changes  []string `json:"changes,omitempty"`
	Evidence string   `json:"evidence,omitempty"`
	TS       float64  `json:"ts"`
	Base     int      `json:"base"`
	Messages int      `json:"messages"`
}

// Feedback is a reply the user marked with /style bad, plus an optional reason.
type Feedback struct {
	TS    float64 `json:"ts"`
	Reply string  `json:"reply"`
	Note  string  `json:"note,omitempty"`
}

type data struct {
	Next     int        `json:"next"`
	Active   Version    `json:"active"`
	History  []Version  `json:"history,omitempty"`
	Pending  *Proposal  `json:"pending,omitempty"`
	Rejected [][]string `json:"rejected,omitempty"`
	Feedback []Feedback `json:"feedback,omitempty"`
}

var (
	ErrNoPending = errors.New("没有待审批的提案")
	ErrStale     = errors.New("提案之后笔记已被改过，这个提案已作废")
	ErrNoHistory = errors.New("没有更早的版本")
)

type Store struct {
	mu   sync.Mutex
	path string
	d    data
	now  func() time.Time
}

// Open loads path; a missing file is an empty store, a corrupt one is moved aside (path.corrupt-<unix>).
func Open(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, now: now}
	b, err := os.ReadFile(path)
	switch {
	case os.IsNotExist(err):
		return s, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &s.d); err != nil {
		bad := fmt.Sprintf("%s.corrupt-%d", path, now().Unix())
		slog.Warn("style.json is corrupt; moved aside and starting with empty style notes", "moved_to", bad, "err", err)
		if rerr := os.Rename(path, bad); rerr != nil {
			return nil, rerr
		}
		s.d = data{}
	}
	return s, nil
}

func (s *Store) ts() float64 { return float64(s.now().UnixMilli()) / 1000 }

func (s *Store) save() error {
	b, err := json.MarshalIndent(s.d, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Active returns the notes currently injected into the prompt.
func (s *Store) Active() Version {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.d.Active
}

// Text is Active().Text, trimmed.
func (s *Store) Text() string { return strings.TrimSpace(s.Active().Text) }

func (s *Store) Pending() *Proposal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.Pending == nil {
		return nil
	}
	p := *s.d.Pending
	p.Changes = append([]string(nil), p.Changes...)
	return &p
}

// setLocked makes text the active version, pushing the old one onto the history.
func (s *Store) setLocked(text, reason string) Version {
	// the empty starting state (v0) is kept too, so the first change can be rolled back
	s.d.History = append(s.d.History, s.d.Active)
	if len(s.d.History) > maxHistory {
		s.d.History = s.d.History[len(s.d.History)-maxHistory:]
	}
	if s.d.Next <= s.d.Active.N {
		s.d.Next = s.d.Active.N
	}
	s.d.Next++
	s.d.Active = Version{N: s.d.Next, Text: strings.TrimSpace(text), TS: s.ts(), Reason: reason}
	return s.d.Active
}

// Set replaces the notes (manual edit, /style clear, or an auto-applied review). A pending proposal is dropped
// because it was computed against the old notes.
func (s *Store) Set(text, reason string) (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.setLocked(text, reason)
	s.d.Pending = nil
	return v, s.save()
}

// Propose stores a review result for approval, replacing any older proposal.
func (s *Store) Propose(p Proposal) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p.TS = s.ts()
	p.Base = s.d.Active.N
	p.Text = strings.TrimSpace(p.Text)
	s.d.Pending = &p
	return s.save()
}

// Approve activates the pending proposal.
func (s *Store) Approve() (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.d.Pending
	if p == nil {
		return Version{}, ErrNoPending
	}
	if p.Base != s.d.Active.N {
		s.d.Pending = nil
		return Version{}, errors.Join(ErrStale, s.save())
	}
	v := s.setLocked(p.Text, "review")
	s.d.Pending = nil
	return v, s.save()
}

// Reject drops the pending proposal and remembers its change list for future reviews.
func (s *Store) Reject() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.d.Pending == nil {
		return ErrNoPending
	}
	if len(s.d.Pending.Changes) > 0 {
		s.d.Rejected = append(s.d.Rejected, s.d.Pending.Changes)
		if len(s.d.Rejected) > maxRejected {
			s.d.Rejected = s.d.Rejected[len(s.d.Rejected)-maxRejected:]
		}
	}
	s.d.Pending = nil
	return s.save()
}

// Rollback restores the previous version (the current one is discarded).
func (s *Store) Rollback() (Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.d.History) == 0 {
		return Version{}, ErrNoHistory
	}
	s.d.Active = s.d.History[len(s.d.History)-1]
	s.d.History = s.d.History[:len(s.d.History)-1]
	s.d.Pending = nil
	return s.d.Active, s.save()
}

func (s *Store) Rejected() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]string, len(s.d.Rejected))
	for i, r := range s.d.Rejected {
		out[i] = append([]string(nil), r...)
	}
	return out
}

func (s *Store) AddFeedback(reply, note string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Feedback = append(s.d.Feedback, Feedback{TS: s.ts(), Reply: reply, Note: strings.TrimSpace(note)})
	if len(s.d.Feedback) > maxFeedback {
		s.d.Feedback = s.d.Feedback[len(s.d.Feedback)-maxFeedback:]
	}
	return len(s.d.Feedback), s.save()
}

func (s *Store) Feedback() []Feedback {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Feedback(nil), s.d.Feedback...)
}

// ConsumeFeedback removes the entries a review has used (those with TS <= upTo); newer ones stay.
func (s *Store) ConsumeFeedback(upTo float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	keep := s.d.Feedback[:0]
	for _, f := range s.d.Feedback {
		if f.TS > upTo {
			keep = append(keep, f)
		}
	}
	s.d.Feedback = keep
	return s.save()
}
