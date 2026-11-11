package pager

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/oarkflow/cqlite/internal/page"
)

type Pager struct {
	mu        sync.Mutex
	path      string
	file      *os.File
	pageSize  int
	free      []page.ID
	freeDirty bool
}

const blobMagic = "SQLTBLB1"

func Open(path string, pageSize int) (*Pager, error) {
	if pageSize == 0 {
		pageSize = page.DefaultSize
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	p := &Pager{path: path, file: f, pageSize: pageSize}
	if err := p.loadFreeList(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return p, nil
}

func (p *Pager) Read(id page.ID) (page.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if id == 0 {
		return page.Page{}, errors.New("page id 0 is reserved")
	}
	buf := make([]byte, p.pageSize)
	_, err := p.file.ReadAt(buf, int64(id-1)*int64(p.pageSize))
	if err != nil {
		if errors.Is(err, io.EOF) {
			return page.New(id, p.pageSize), nil
		}
		return page.Page{}, err
	}
	pg, err := page.Decode(buf)
	if err != nil {
		return page.Page{}, err
	}
	if pg.ID != id {
		return page.Page{}, errors.New("page id mismatch")
	}
	return pg, nil
}

func (p *Pager) Write(pg page.Page) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if pg.ID == 0 {
		return errors.New("page id 0 is reserved")
	}
	buf, err := page.Encode(pg, p.pageSize)
	if err != nil {
		return err
	}
	_, err = p.file.WriteAt(buf, int64(pg.ID-1)*int64(p.pageSize))
	return err
}

func (p *Pager) Allocate() (page.ID, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n := len(p.free); n > 0 {
		id := p.free[n-1]
		p.free = p.free[:n-1]
		p.freeDirty = true
		if err := p.persistFreeListLocked(); err != nil {
			return 0, err
		}
		return id, nil
	}
	info, err := p.file.Stat()
	if err != nil {
		return 0, err
	}
	return page.ID(info.Size()/int64(p.pageSize) + 1), nil
}

func (p *Pager) Free(id page.ID) error {
	if id == 0 {
		return errors.New("page id 0 is reserved")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, existing := range p.free {
		if existing == id {
			return nil
		}
	}
	p.free = append(p.free, id)
	p.freeDirty = true
	return p.persistFreeListLocked()
}

func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.file.Sync()
}

func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.persistFreeListLocked(); err != nil {
		_ = p.file.Close()
		return err
	}
	return p.file.Close()
}

func (p *Pager) loadFreeList() error {
	data, err := os.ReadFile(p.path + ".free")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	var raw []uint64
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	p.free = make([]page.ID, len(raw))
	for i, id := range raw {
		p.free[i] = page.ID(id)
	}
	return nil
}

func (p *Pager) persistFreeListLocked() error {
	if !p.freeDirty {
		return nil
	}
	raw := make([]uint64, len(p.free))
	for i, id := range p.free {
		raw[i] = uint64(id)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	tmp := p.path + ".free.tmp"
	if err := writeFileSynced(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.path+".free"); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(p.path)); err != nil {
		return err
	}
	p.freeDirty = false
	return nil
}

func (p *Pager) WriteBlob(data []byte) error {
	chunkSize := p.pageSize - page.HeaderSize - 16
	if chunkSize <= 0 {
		return errors.New("page size too small for blob")
	}
	count := (len(data) + chunkSize - 1) / chunkSize
	if count == 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		start := i * chunkSize
		end := start + chunkSize
		if end > len(data) {
			end = len(data)
		}
		pg := page.New(page.ID(i+1), p.pageSize)
		copy(pg.Data[0:8], []byte(blobMagic))
		if i == 0 {
			binary.LittleEndian.PutUint64(pg.Data[8:16], uint64(len(data)))
		}
		copy(pg.Data[16:], data[start:end])
		if err := p.Write(pg); err != nil {
			return err
		}
	}
	if err := p.Sync(); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p.path))
}

func (p *Pager) ReadBlob() ([]byte, error) {
	first, err := p.Read(1)
	if err != nil {
		return nil, err
	}
	if string(first.Data[0:8]) != blobMagic {
		return nil, os.ErrNotExist
	}
	total := int(binary.LittleEndian.Uint64(first.Data[8:16]))
	if total == 0 {
		return nil, nil
	}
	chunkSize := p.pageSize - page.HeaderSize - 16
	out := make([]byte, 0, total)
	count := (total + chunkSize - 1) / chunkSize
	for i := 0; i < count; i++ {
		pg := first
		if i > 0 {
			pg, err = p.Read(page.ID(i + 1))
			if err != nil {
				return nil, err
			}
		}
		if string(pg.Data[0:8]) != blobMagic {
			return nil, errors.New("invalid blob page")
		}
		remaining := total - len(out)
		n := chunkSize
		if remaining < n {
			n = remaining
		}
		out = append(out, pg.Data[16:16+n]...)
	}
	return out, nil
}

func writeFileSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if n, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	} else if n != len(data) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDirFromFile(path string) error {
	return syncDir(filepath.Dir(path))
}
