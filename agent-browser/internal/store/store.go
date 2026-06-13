package store

import (
	"sync"
	"time"

	"github.com/revitt/agent-browser/internal/snapshot"
)

type ElementRef struct {
	TabID     string
	Ref       string
	Role      string
	Name      string
	Tag       string
	Type      string
	Key       string
	UpdatedAt time.Time
}

type RefStore struct {
	mu     sync.RWMutex
	byTab  map[string]map[string]ElementRef
	active string
}

func New() *RefStore {
	return &RefStore{byTab: map[string]map[string]ElementRef{}}
}

func (s *RefStore) Observe(tabID string, elements []snapshot.Element) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.byTab[tabID] == nil {
		s.byTab[tabID] = map[string]ElementRef{}
	}
	now := time.Now()
	for _, el := range elements {
		if el.Ref == "" {
			continue
		}
		s.byTab[tabID][el.Ref] = ElementRef{
			TabID:     tabID,
			Ref:       el.Ref,
			Role:      el.Role,
			Name:      el.Name,
			Tag:       el.Tag,
			Type:      el.Type,
			Key:       el.Key,
			UpdatedAt: now,
		}
	}
}

func (s *RefStore) Get(tabID, ref string) (ElementRef, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tab := s.byTab[tabID]
	if tab == nil {
		return ElementRef{}, false
	}
	el, ok := tab[ref]
	return el, ok
}

func (s *RefStore) DropTab(tabID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byTab, tabID)
	if s.active == tabID {
		s.active = ""
	}
}

func (s *RefStore) SetActive(tabID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.active = tabID
}

func (s *RefStore) Active() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.active
}
