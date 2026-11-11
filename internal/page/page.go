package page

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
)

const (
	DefaultSize = 4096
	HeaderSize  = 24
)

type ID uint64

type Page struct {
	ID   ID
	LSN  uint64
	Data []byte
}

func New(id ID, size int) Page {
	if size <= HeaderSize {
		size = DefaultSize
	}
	return Page{ID: id, Data: make([]byte, size-HeaderSize)}
}

func Encode(p Page, size int) ([]byte, error) {
	if size <= HeaderSize {
		return nil, errors.New("page size too small")
	}
	if len(p.Data) > size-HeaderSize {
		return nil, errors.New("page payload too large")
	}
	buf := make([]byte, size)
	if err := EncodeInto(p, size, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// EncodeInto serializes a page into caller-owned storage. Reusing buf avoids a
// page-sized allocation for every page written during checkpointing.
func EncodeInto(p Page, size int, buf []byte) error {
	if size <= HeaderSize {
		return errors.New("page size too small")
	}
	if len(p.Data) > size-HeaderSize {
		return errors.New("page payload too large")
	}
	if len(buf) < size {
		return errors.New("page output buffer too small")
	}
	buf = buf[:size]
	clear(buf)
	binary.LittleEndian.PutUint64(buf[0:8], uint64(p.ID))
	binary.LittleEndian.PutUint64(buf[8:16], p.LSN)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(p.Data)))
	copy(buf[HeaderSize:], p.Data)
	sum := crc32.Update(0, crc32.IEEETable, buf[8:16])
	sum = crc32.Update(sum, crc32.IEEETable, buf[20:])
	binary.LittleEndian.PutUint32(buf[16:20], sum)
	return nil
}

func Decode(buf []byte) (Page, error) {
	if len(buf) <= HeaderSize {
		return Page{}, errors.New("page buffer too small")
	}
	want := binary.LittleEndian.Uint32(buf[16:20])
	got := crc32.Update(0, crc32.IEEETable, buf[8:16])
	got = crc32.Update(got, crc32.IEEETable, buf[20:])
	if got != want {
		return Page{}, errors.New("page checksum mismatch")
	}
	n := int(binary.LittleEndian.Uint32(buf[20:24]))
	if n < 0 || n > len(buf)-HeaderSize {
		return Page{}, errors.New("invalid page payload length")
	}
	data := make([]byte, n)
	copy(data, buf[HeaderSize:HeaderSize+n])
	return Page{
		ID:   ID(binary.LittleEndian.Uint64(buf[0:8])),
		LSN:  binary.LittleEndian.Uint64(buf[8:16]),
		Data: data,
	}, nil
}
