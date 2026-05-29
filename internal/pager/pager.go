package pager

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/sujit/sqlite/internal/page"
)

type Pager struct {
	mu       sync.Mutex
	file     *os.File
	pageSize int
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
	return &Pager{file: f, pageSize: pageSize}, nil
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
	return page.Decode(buf)
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
	info, err := p.file.Stat()
	if err != nil {
		return 0, err
	}
	return page.ID(info.Size()/int64(p.pageSize) + 1), nil
}

func (p *Pager) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.file.Sync()
}

func (p *Pager) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.file.Close()
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
	return p.Sync()
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
