// Package cache holds the latest rendered payload per device so the hot GET path never
// touches the calendar API or the renderer — it just serves bytes (or a 304).
package cache

import (
	"sync"
	"time"

	"github.com/Comradery64/reTerminal-eink-signage/backend/internal/render"
)

type Entry struct {
	Payload    render.Payload
	ETag       string // quoted hex CRC32, ready for the ETag header
	RenderedAt time.Time
	Err        error // last poll error, if the room is currently failing
}

type Store struct {
	mu sync.RWMutex
	m  map[string]Entry // keyed by device_id
}

func New() *Store { return &Store{m: make(map[string]Entry)} }

func (s *Store) Get(deviceID string) (Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.m[deviceID]
	return e, ok
}

func (s *Store) Set(deviceID string, e Entry) {
	s.mu.Lock()
	s.m[deviceID] = e
	s.mu.Unlock()
}

func (s *Store) SetError(deviceID string, err error) {
	s.mu.Lock()
	e := s.m[deviceID]
	e.Err = err
	s.m[deviceID] = e
	s.mu.Unlock()
}
