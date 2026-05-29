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
	binary.LittleEndian.PutUint64(buf[0:8], uint64(p.ID))
	binary.LittleEndian.PutUint64(buf[8:16], p.LSN)
	binary.LittleEndian.PutUint32(buf[20:24], uint32(len(p.Data)))
	copy(buf[HeaderSize:], p.Data)
	check := make([]byte, 0, len(buf)-8)
	check = append(check, buf[8:16]...)
	check = append(check, buf[20:]...)
	sum := crc32.ChecksumIEEE(check)
	binary.LittleEndian.PutUint32(buf[16:20], sum)
	return buf, nil
}

func Decode(buf []byte) (Page, error) {
	if len(buf) <= HeaderSize {
		return Page{}, errors.New("page buffer too small")
	}
	want := binary.LittleEndian.Uint32(buf[16:20])
	check := make([]byte, 0, len(buf)-8)
	check = append(check, buf[8:16]...)
	check = append(check, buf[20:]...)
	got := crc32.ChecksumIEEE(check)
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
