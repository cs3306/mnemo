// Package store persists everything the bot knows, as plain files in one directory:
//
//	messages.jsonl   append-only raw chat log (provenance; never sent to the model wholesale)
//	memories.jsonl   append-only event log of memory changes, replayed on start
//	state.json       small atomic snapshot: core profile, summaries, learning, pins, cursors
//	tg_offset        Telegram update offset
//
// Memories are plain text; each carries the embedding model that produced its vector and the chat
// model that extracted it, so memory survives swapping either model (see Reembed / Export / Import).
// Single-writer: run one bot process per data directory.
package store

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1

// Now returns unix seconds; tests override it.
var Now = func() float64 { return float64(time.Now().UnixNano()) / 1e9 }

// Vec is a float32 vector serialised as base64 little-endian bytes.
type Vec []float32

func (v Vec) MarshalJSON() ([]byte, error) {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(b))
}

func (v *Vec) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	if len(b)%4 != 0 {
		return errors.New("vector byte length not a multiple of 4")
	}
	out := make(Vec, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	*v = out
	return nil
}

type Message struct {
	ID      int64   `json:"id"`
	Chat    int64   `json:"chat"`
	Role    string  `json:"role"`
	Content string  `json:"content"`
	TS      float64 `json:"ts"`
}

type Memory struct {
	ID           int64   `json:"id"`
	Kind         string  `json:"kind"`
	Text         string  `json:"text"`
	Importance   int     `json:"importance"`
	Created      float64 `json:"created"`
	LastUsed     float64 `json:"last_used,omitempty"`
	UseCount     int     `json:"use_count,omitempty"`
	Status       string  `json:"status"` // active | superseded | archived
	SupersededBy int64   `json:"superseded_by,omitempty"`
	Source       string  `json:"source"`
	ByModel      string  `json:"by_model,omitempty"`    // chat model that extracted it
	EmbedModel   string  `json:"embed_model,omitempty"` // embedding model that produced Emb
	SrcMsg       int64   `json:"src_msg,omitempty"`
	Emb          Vec     `json:"emb,omitempty"`
}

type Learning struct {
	Topic        string  `json:"topic"`
	Status       string  `json:"status"` // weak | learning | solid
	ErrorPattern string  `json:"error_pattern,omitempty"`
	LastSeen     float64 `json:"last_seen"`
}

type CoreVersion struct {
	Text   string  `json:"text"`
	Reason string  `json:"reason,omitempty"`
	TS     float64 `json:"ts"`
}

type state struct {
	SchemaVersion  int                  `json:"schema_version"`
	Core           string               `json:"core"`
	CoreHistory    []CoreVersion        `json:"core_history,omitempty"`
	Summaries      map[int64]string     `json:"summaries,omitempty"`
	SummarizedUpTo map[int64]int64      `json:"summarized_up_to,omitempty"`
	ExtractedUpTo  map[int64]int64      `json:"extracted_up_to,omitempty"`
	Learning       map[string]*Learning `json:"learning,omitempty"`
	Pins           []string             `json:"pins,omitempty"`
	Meta           map[string]string    `json:"meta,omitempty"`
}

type event struct {
	Op         string  `json:"op"` // add | touch | bump | supersede | status | edit
	Mem        *Memory `json:"mem,omitempty"`
	ID         int64   `json:"id,omitempty"`
	IDs        []int64 `json:"ids,omitempty"`
	TS         float64 `json:"ts,omitempty"`
	Importance int     `json:"importance,omitempty"`
	By         int64   `json:"by,omitempty"`
	Status     string  `json:"status,omitempty"`
	Text       string  `json:"text,omitempty"`
	EmbedModel string  `json:"embed_model,omitempty"`
	Emb        Vec     `json:"emb,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	dir      string
	readOnly bool
	msgs     map[int64][]Message
	nextMsg  int64
	mems     map[int64]*Memory
	nextMem  int64
	st       state
	msgFile  *os.File
	memFile  *os.File
}

// Open opens (creating if needed) a read-write store.
func Open(dir string) (*Store, error) { return open(dir, false) }

// OpenReadOnly loads the data without opening files for writing (used by `export`).
func OpenReadOnly(dir string) (*Store, error) { return open(dir, true) }

func open(dir string, readOnly bool) (*Store, error) {
	if !readOnly {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	s := &Store{dir: dir, readOnly: readOnly, msgs: map[int64][]Message{}, mems: map[int64]*Memory{}, nextMsg: 1, nextMem: 1}
	s.st = state{SchemaVersion: SchemaVersion}
	if err := s.load(); err != nil {
		return nil, err
	}
	s.initMaps()
	if !readOnly {
		var err error
		if s.msgFile, err = os.OpenFile(s.path("messages.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
			return nil, err
		}
		if s.memFile, err = os.OpenFile(s.path("memories.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) path(name string) string { return filepath.Join(s.dir, name) }

func (s *Store) initMaps() {
	if s.st.Summaries == nil {
		s.st.Summaries = map[int64]string{}
	}
	if s.st.SummarizedUpTo == nil {
		s.st.SummarizedUpTo = map[int64]int64{}
	}
	if s.st.ExtractedUpTo == nil {
		s.st.ExtractedUpTo = map[int64]int64{}
	}
	if s.st.Learning == nil {
		s.st.Learning = map[string]*Learning{}
	}
	if s.st.Meta == nil {
		s.st.Meta = map[string]string{}
	}
}

// readLines calls fn for each non-empty line; a malformed final line (crash mid-write) is tolerated.
func readLines(path string, fn func([]byte) error) error {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	bad := 0
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			if ferr := fn(line); ferr != nil {
				bad++
				slog.Warn("skipping malformed record", "file", filepath.Base(path))
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if bad > 1 {
		return fmt.Errorf("%s: %d malformed records; refusing to continue", filepath.Base(path), bad)
	}
	return nil
}

func (s *Store) load() error {
	if b, err := os.ReadFile(s.path("state.json")); err == nil {
		if err := json.Unmarshal(b, &s.st); err != nil {
			return fmt.Errorf("state.json: %w", err)
		}
		if s.st.SchemaVersion > SchemaVersion {
			return fmt.Errorf("state.json schema %d is newer than this build (%d)", s.st.SchemaVersion, SchemaVersion)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := readLines(s.path("messages.jsonl"), func(b []byte) error {
		var m Message
		if err := json.Unmarshal(b, &m); err != nil {
			return err
		}
		s.msgs[m.Chat] = append(s.msgs[m.Chat], m)
		if m.ID >= s.nextMsg {
			s.nextMsg = m.ID + 1
		}
		return nil
	}); err != nil {
		return err
	}
	return readLines(s.path("memories.jsonl"), func(b []byte) error {
		var ev event
		if err := json.Unmarshal(b, &ev); err != nil {
			return err
		}
		s.apply(ev)
		return nil
	})
}

func (s *Store) apply(ev event) {
	switch ev.Op {
	case "add":
		if ev.Mem == nil {
			return
		}
		m := *ev.Mem
		s.mems[m.ID] = &m
		if m.ID >= s.nextMem {
			s.nextMem = m.ID + 1
		}
	case "touch":
		for _, id := range ev.IDs {
			if m := s.mems[id]; m != nil {
				m.LastUsed = ev.TS
				m.UseCount++
			}
		}
	case "bump":
		if m := s.mems[ev.ID]; m != nil {
			if ev.Importance > m.Importance {
				m.Importance = ev.Importance
			}
			m.LastUsed = ev.TS
		}
	case "supersede":
		if m := s.mems[ev.ID]; m != nil {
			m.Status, m.SupersededBy = "superseded", ev.By
		}
	case "status":
		if m := s.mems[ev.ID]; m != nil {
			m.Status = ev.Status
		}
	case "edit":
		if m := s.mems[ev.ID]; m != nil {
			m.Text, m.EmbedModel, m.Emb = ev.Text, ev.EmbedModel, ev.Emb
		}
	}
}

func appendJSON(f *os.File, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (s *Store) writeOnly() error {
	if s.readOnly {
		return errors.New("store opened read-only")
	}
	return nil
}

// saveState writes the snapshot atomically (temp file + rename).
func (s *Store) saveState() error {
	if err := s.writeOnly(); err != nil {
		return err
	}
	b, err := json.Marshal(s.st)
	if err != nil {
		return err
	}
	return atomicWrite(s.path("state.json"), b)
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Close flushes and closes the log files.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first error
	for _, f := range []*os.File{s.msgFile, s.memFile} {
		if f != nil {
			if err := f.Close(); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// ---------------------------------------------------------------- messages

func (s *Store) AddMessage(chat int64, role, content string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeOnly(); err != nil {
		return 0, err
	}
	m := Message{ID: s.nextMsg, Chat: chat, Role: role, Content: content, TS: Now()}
	if err := appendJSON(s.msgFile, m); err != nil {
		return 0, err
	}
	s.nextMsg++
	s.msgs[chat] = append(s.msgs[chat], m)
	return m.ID, nil
}

// Unsummarized returns the chat's messages not yet folded into the rolling summary, oldest first.
func (s *Store) Unsummarized(chat int64) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.afterLocked(chat, s.st.SummarizedUpTo[chat], 0)
}

func (s *Store) MessagesAfter(chat, after int64, limit int) []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.afterLocked(chat, after, limit)
}

func (s *Store) afterLocked(chat, after int64, limit int) []Message {
	all := s.msgs[chat]
	i := sort.Search(len(all), func(i int) bool { return all[i].ID > after })
	out := all[i:]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return append([]Message(nil), out...)
}

func (s *Store) LastMessageTS(chat int64) float64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if all := s.msgs[chat]; len(all) > 0 {
		return all[len(all)-1].TS
	}
	return 0
}

func (s *Store) ChatIDs() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]int64, 0, len(s.msgs))
	for id := range s.msgs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// LastByRole returns the most recent message with the given role and ID < before (before<=0: any).
func (s *Store) LastByRole(chat int64, role string, before int64) (Message, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.msgs[chat]
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].Role == role && (before <= 0 || all[i].ID < before) {
			return all[i], true
		}
	}
	return Message{}, false
}

// ---------------------------------------------------------------- summary / cursors / core

func (s *Store) Summary(chat int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Summaries[chat]
}

// SetSummary stores the rolling summary and marks messages up to upTo as folded into it.
func (s *Store) SetSummary(chat int64, text string, upTo int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Summaries[chat] = text
	if upTo > s.st.SummarizedUpTo[chat] {
		s.st.SummarizedUpTo[chat] = upTo
	}
	return s.saveState()
}

// ResetChat starts the conversation over without deleting anything: the rolling summary is cleared and both cursors move
// to the newest message, so the recent window is empty and the old turns are neither replayed nor mined for memories.
// Memories, the core profile and pinned style examples are untouched; messages.jsonl keeps every message.
func (s *Store) ResetChat(chat int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var last int64
	if all := s.msgs[chat]; len(all) > 0 {
		last = all[len(all)-1].ID
	}
	n := len(s.afterLocked(chat, s.st.SummarizedUpTo[chat], 0))
	delete(s.st.Summaries, chat)
	s.st.SummarizedUpTo[chat] = last
	s.st.ExtractedUpTo[chat] = last
	return n, s.saveState()
}

func (s *Store) ExtractedUpTo(chat int64) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.ExtractedUpTo[chat]
}

func (s *Store) SetExtractedUpTo(chat, id int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.ExtractedUpTo[chat] = id
	return s.saveState()
}

func (s *Store) Core() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Core
}

func (s *Store) SetCore(text, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Core != "" && s.st.Core != text {
		s.st.CoreHistory = append(s.st.CoreHistory, CoreVersion{Text: s.st.Core, Reason: reason, TS: Now()})
		if n := len(s.st.CoreHistory); n > 20 {
			s.st.CoreHistory = s.st.CoreHistory[n-20:]
		}
	}
	s.st.Core = text
	return s.saveState()
}

func (s *Store) Meta(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.Meta[key]
}

func (s *Store) SetMeta(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Meta[key] = value
	return s.saveState()
}

func (s *Store) MetaFloat(key string) float64 {
	f, _ := strconv.ParseFloat(s.Meta(key), 64)
	return f
}

// ---------------------------------------------------------------- pins (style anchors)

func (s *Store) Pins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.st.Pins...)
}

func (s *Store) AddPin(text string, max int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.Pins = append(s.st.Pins, text)
	if max > 0 && len(s.st.Pins) > max {
		s.st.Pins = s.st.Pins[len(s.st.Pins)-max:]
	}
	return s.saveState()
}

// RemovePin removes the pin at 1-based index n.
func (s *Store) RemovePin(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n < 1 || n > len(s.st.Pins) {
		return false
	}
	s.st.Pins = append(s.st.Pins[:n-1], s.st.Pins[n:]...)
	return s.saveState() == nil
}

// ---------------------------------------------------------------- learning progress

func (s *Store) UpsertLearning(topic, status, errorPattern string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.st.Learning[topic]
	if l == nil {
		l = &Learning{Topic: topic}
		s.st.Learning[topic] = l
	}
	l.Status, l.LastSeen = status, Now()
	if errorPattern != "" {
		l.ErrorPattern = errorPattern
	}
	return s.saveState()
}

func (s *Store) ListLearning(limit int) []Learning {
	s.mu.Lock()
	defer s.mu.Unlock()
	rank := map[string]int{"weak": 0, "learning": 1, "solid": 2}
	out := make([]Learning, 0, len(s.st.Learning))
	for _, l := range s.st.Learning {
		out = append(out, *l)
	}
	sort.Slice(out, func(i, j int) bool {
		if rank[out[i].Status] != rank[out[j].Status] {
			return rank[out[i].Status] < rank[out[j].Status]
		}
		return out[i].LastSeen > out[j].LastSeen
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// ---------------------------------------------------------------- Telegram offset

func (s *Store) Offset() int64 {
	b, err := os.ReadFile(s.path("tg_offset"))
	if err != nil {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n
}

func (s *Store) SetOffset(n int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeOnly(); err != nil {
		return err
	}
	return atomicWrite(s.path("tg_offset"), []byte(strconv.FormatInt(n, 10)))
}

// ---------------------------------------------------------------- memories

func (s *Store) emit(ev event) error {
	if err := s.writeOnly(); err != nil {
		return err
	}
	if err := appendJSON(s.memFile, ev); err != nil {
		return err
	}
	s.apply(ev)
	return nil
}

// AddMemory assigns an ID and persists the memory.
func (s *Store) AddMemory(m Memory) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m.ID = s.nextMem
	if m.Status == "" {
		m.Status = "active"
	}
	if m.Source == "" {
		m.Source = "user"
	}
	if m.Importance < 1 {
		m.Importance = 3
	}
	if m.Importance > 5 {
		m.Importance = 5
	}
	if m.Created == 0 {
		m.Created = Now()
	}
	if err := s.emit(event{Op: "add", Mem: &m}); err != nil {
		return 0, err
	}
	return m.ID, nil
}

func (s *Store) GetMemory(id int64) (Memory, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.mems[id]; m != nil {
		return *m, true
	}
	return Memory{}, false
}

func (s *Store) filter(keep func(*Memory) bool) []Memory {
	out := make([]Memory, 0, len(s.mems))
	for _, m := range s.mems {
		if keep(m) {
			out = append(out, *m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ActiveMemories returns active memories ordered by ID. Emb slices are shared and must not be mutated.
func (s *Store) ActiveMemories() []Memory {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filter(func(m *Memory) bool { return m.Status == "active" })
}

func (s *Store) AllMemories() []Memory {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.filter(func(*Memory) bool { return true })
}

func (s *Store) TouchMemories(ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit(event{Op: "touch", IDs: ids, TS: Now()})
}

func (s *Store) BumpImportance(id int64, importance int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit(event{Op: "bump", ID: id, Importance: importance, TS: Now()})
}

func (s *Store) Supersede(oldID, newID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit(event{Op: "supersede", ID: oldID, By: newID})
}

func (s *Store) SetStatus(id int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.emit(event{Op: "status", ID: id, Status: status})
}

// EditMemory replaces text and vector (and the embedding model that produced it).
func (s *Store) EditMemory(id int64, text, embedModel string, emb []float32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mems[id] == nil {
		return fmt.Errorf("memory %d not found", id)
	}
	return s.emit(event{Op: "edit", ID: id, Text: text, EmbedModel: embedModel, Emb: emb})
}

// DeleteMemory removes a memory for good: the log is rewritten without it immediately.
func (s *Store) DeleteMemory(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mems[id] == nil {
		return false, nil
	}
	delete(s.mems, id)
	return true, s.compactLocked()
}

// Compact rewrites memories.jsonl as one "add" event per current memory (drops history).
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.compactLocked()
}

func (s *Store) compactLocked() error {
	if err := s.writeOnly(); err != nil {
		return err
	}
	ids := make([]int64, 0, len(s.mems))
	for id := range s.mems {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var buf bytes.Buffer
	for _, id := range ids {
		b, err := json.Marshal(event{Op: "add", Mem: s.mems[id]})
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if err := s.memFile.Close(); err != nil {
		return err
	}
	if err := atomicWrite(s.path("memories.jsonl"), buf.Bytes()); err != nil {
		return err
	}
	f, err := os.OpenFile(s.path("memories.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	s.memFile = f
	return nil
}

func (s *Store) CountMemories() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]int{}
	for _, m := range s.mems {
		out[m.Status]++
	}
	return out
}

// ---------------------------------------------------------------- backup

// Backup copies the data files into dir/<timestamp>/ and keeps the newest `keep` backups.
func (s *Store) Backup(dir string, keep int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeOnly(); err != nil {
		return "", err
	}
	dest := filepath.Join(dir, time.Now().UTC().Format("20060102-150405.000"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	for _, name := range []string{"messages.jsonl", "memories.jsonl", "state.json"} {
		b, err := os.ReadFile(s.path(name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(dest, name), b, 0o600); err != nil {
			return "", err
		}
	}
	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for len(names) > keep {
		_ = os.RemoveAll(filepath.Join(dir, names[0]))
		names = names[1:]
	}
	return dest, nil
}

// ---------------------------------------------------------------- export / import (model-independent)

type ExportMemory struct {
	Kind       string  `json:"kind"`
	Text       string  `json:"text"`
	Importance int     `json:"importance"`
	Created    float64 `json:"created"`
	Status     string  `json:"status"`
	Source     string  `json:"source,omitempty"`
	ByModel    string  `json:"by_model,omitempty"`
}

// Export is a plain-text snapshot with no vectors: it can be imported into any embedding setup.
type Export struct {
	Version    int              `json:"version"`
	ExportedAt string           `json:"exported_at"`
	Core       string           `json:"core"`
	Summaries  map[int64]string `json:"summaries,omitempty"`
	Learning   []Learning       `json:"learning,omitempty"`
	Pins       []string         `json:"pins,omitempty"`
	Memories   []ExportMemory   `json:"memories"`
}

func (s *Store) Export() Export {
	s.mu.Lock()
	defer s.mu.Unlock()
	ex := Export{Version: SchemaVersion, ExportedAt: time.Now().UTC().Format(time.RFC3339), Core: s.st.Core,
		Summaries: map[int64]string{}, Pins: append([]string(nil), s.st.Pins...)}
	for k, v := range s.st.Summaries {
		ex.Summaries[k] = v
	}
	for _, l := range s.st.Learning {
		ex.Learning = append(ex.Learning, *l)
	}
	sort.Slice(ex.Learning, func(i, j int) bool { return ex.Learning[i].Topic < ex.Learning[j].Topic })
	for _, m := range s.filter(func(m *Memory) bool { return m.Status != "superseded" }) {
		ex.Memories = append(ex.Memories, ExportMemory{Kind: m.Kind, Text: m.Text, Importance: m.Importance,
			Created: m.Created, Status: m.Status, Source: m.Source, ByModel: m.ByModel})
	}
	return ex
}

// Import adds exported memories (without vectors, so they await Reembed) and merges core/learning/pins
// where the target is empty. Exact-duplicate texts are skipped. Returns the number of memories added.
func (s *Store) Import(ex Export) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeOnly(); err != nil {
		return 0, err
	}
	have := map[string]bool{}
	for _, m := range s.mems {
		have[m.Text] = true
	}
	added := 0
	for _, em := range ex.Memories {
		if strings.TrimSpace(em.Text) == "" || have[em.Text] {
			continue
		}
		m := Memory{ID: s.nextMem, Kind: em.Kind, Text: em.Text, Importance: em.Importance, Created: em.Created,
			Status: em.Status, Source: em.Source, ByModel: em.ByModel}
		if m.Status == "" {
			m.Status = "active"
		}
		if m.Importance < 1 || m.Importance > 5 {
			m.Importance = 3
		}
		if m.Created == 0 {
			m.Created = Now()
		}
		if err := s.emit(event{Op: "add", Mem: &m}); err != nil {
			return added, err
		}
		have[em.Text] = true
		added++
	}
	if s.st.Core == "" {
		s.st.Core = ex.Core
	}
	for _, l := range ex.Learning {
		if _, ok := s.st.Learning[l.Topic]; !ok {
			l := l
			s.st.Learning[l.Topic] = &l
		}
	}
	if len(s.st.Pins) == 0 {
		s.st.Pins = ex.Pins
	}
	return added, s.saveState()
}
