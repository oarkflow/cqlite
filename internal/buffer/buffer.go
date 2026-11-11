package buffer

import (
	"container/list"
	"errors"
	"sync"

	"github.com/oarkflow/cqlite/internal/page"
)

type Frame struct {
	Page  page.Page
	Dirty bool
	Pins  int
}

type Pool struct {
	mu       sync.Mutex
	capacity int
	frames   map[page.ID]*list.Element
	lru      *list.List
}

func New(capacity int) *Pool {
	if capacity <= 0 {
		capacity = 128
	}
	return &Pool{capacity: capacity, frames: make(map[page.ID]*list.Element), lru: list.New()}
}

func (p *Pool) Pin(pg page.Page) (*Frame, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if el, ok := p.frames[pg.ID]; ok {
		fr := el.Value.(*Frame)
		fr.Pins++
		p.lru.MoveToFront(el)
		return fr, nil
	}
	if len(p.frames) >= p.capacity {
		if err := p.evict(); err != nil {
			return nil, err
		}
	}
	fr := &Frame{Page: pg, Pins: 1}
	p.frames[pg.ID] = p.lru.PushFront(fr)
	return fr, nil
}

func (p *Pool) Unpin(id page.ID, dirty bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	el, ok := p.frames[id]
	if !ok {
		return errors.New("page not pinned")
	}
	fr := el.Value.(*Frame)
	if fr.Pins == 0 {
		return errors.New("page pin count underflow")
	}
	fr.Pins--
	fr.Dirty = fr.Dirty || dirty
	return nil
}

func (p *Pool) evict() error {
	for el := p.lru.Back(); el != nil; el = el.Prev() {
		fr := el.Value.(*Frame)
		if fr.Pins == 0 && !fr.Dirty {
			delete(p.frames, fr.Page.ID)
			p.lru.Remove(el)
			return nil
		}
	}
	return errors.New("no clean unpinned frame available")
}
