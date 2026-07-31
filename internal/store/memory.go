package store

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests.
//
// It lives in the package rather than in a _test.go file so that the controller and garbage
// collector tests can use it too, without either of them needing a real disk or an S3 endpoint
// to exercise logic that has nothing to do with storage.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject

	// Now, if set, supplies ModTime. Garbage collection has a grace period keyed on age, and
	// testing that honestly requires objects that can be made to look old.
	Now func() time.Time
}

type memObject struct {
	data    []byte
	modTime time.Time
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{objects: make(map[string]memObject)}
}

func (m *Memory) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Memory) Stat(_ context.Context, key string) (Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return Info{}, ErrNotFound
	}
	return Info{Key: key, Size: int64(len(o.data)), ModTime: o.modTime}, nil
}

func (m *Memory) Open(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	o, ok := m.objects[key]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

func (m *Memory) Write(_ context.Context, key string, r io.Reader) error {
	// Read outside the lock: callers stream real content through here and holding the lock for
	// the duration would serialise every write in the process.
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, modTime: m.now()}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}

func (m *Memory) List(_ context.Context, prefix string) ([]Info, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Info
	for k, o := range m.objects {
		if prefix != "" && !strings.HasPrefix(k, prefix) {
			continue
		}
		out = append(out, Info{Key: k, Size: int64(len(o.data)), ModTime: o.modTime})
	}
	return out, nil
}

// SetModTime backdates an object so age-dependent behaviour can be tested without sleeping.
func (m *Memory) SetModTime(key string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if o, ok := m.objects[key]; ok {
		o.modTime = t
		m.objects[key] = o
	}
}

// Len reports how many objects are stored.
func (m *Memory) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.objects)
}
