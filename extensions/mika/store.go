package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type writeReceipt struct {
	Key       string    `json:"key"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"started_at"`
	Text      string    `json:"text,omitempty"`
}

type writeStore struct {
	mu      sync.Mutex
	path    string
	entries map[string]writeReceipt
}

func newWriteStore(path string) *writeStore {
	return &writeStore{path: path, entries: map[string]writeReceipt{}}
}

func openWriteStore(dir string) (*writeStore, error) {
	if dir == "" {
		return newWriteStore(""), nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	s := newWriteStore(filepath.Join(dir, "write-receipts.json"))
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(data) > 2<<20 || json.Unmarshal(data, &s.entries) != nil || s.entries == nil {
		return nil, errors.New("Mika write receipts are invalid")
	}
	for id, r := range s.entries {
		if len(id) == 0 || len(id) > 128 || (r.Status != "pending" && r.Status != "done") {
			return nil, errors.New("invalid Mika write receipt")
		}
	}
	return s, nil
}

func (s *writeStore) commit(id, key string, fn func() (string, error)) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.entries[id]; ok {
		if old.Key != key {
			return "", errors.New("request_id was already used for different Mika parameters")
		}
		if old.Status == "done" {
			return old.Text, nil
		}
		return "", errors.New("the previous Mika action has an unknown outcome; inspect task status before retrying")
	}
	if len(s.entries) >= 256 {
		return "", errors.New("Mika write receipt history is full; preserve it before more actions")
	}
	s.entries[id] = writeReceipt{Key: key, Status: "pending", StartedAt: time.Now().UTC()}
	if err := s.write(); err != nil {
		delete(s.entries, id)
		return "", err
	}
	text, err := fn()
	if err != nil {
		if isDefiniteRefusal(err) {
			delete(s.entries, id)
			_ = s.write()
			return "", err
		}
		return "", fmt.Errorf("Mika action outcome is unknown; inspect task status before retrying: %w", err)
	}
	s.entries[id] = writeReceipt{Key: key, Status: "done", StartedAt: time.Now().UTC(), Text: text}
	if err := s.write(); err != nil {
		return "", fmt.Errorf("the Mika action may have completed; inspect task status before retrying: %w", err)
	}
	return text, nil
}

// isDefiniteRefusal identifies explicit 4xx server refusals where Mika did not accept the mutation.
func isDefiniteRefusal(err error) bool {
	return errors.Is(err, errInvalidArgument) ||
		errors.Is(err, errUnauthorized) ||
		errors.Is(err, errForbidden) ||
		errors.Is(err, errNotFound) ||
		errors.Is(err, errConflict) ||
		errors.Is(err, errGone) ||
		errors.Is(err, errRateLimited) ||
		errors.Is(err, errRedirect)
}

func (s *writeStore) write() error {
	if s.path == "" {
		return nil
	}
	data, err := json.Marshal(s.entries)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".receipts-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err := f.Chmod(0600); err != nil {
		f.Close()
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
	if err := os.Rename(name, s.path); err != nil {
		return err
	}
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
