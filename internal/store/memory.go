package store

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"
)

// Memory is an in-memory Store for tests. It is a non-test file so other packages' tests can use
// it too.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memObject
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
	// Read outside the lock, so a slow reader does not serialise every write.
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{data: data, modTime: time.Now()}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
	return nil
}
