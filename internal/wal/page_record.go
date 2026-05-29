package wal

import (
	"encoding/binary"
	"errors"
)

type PageRecord struct {
	PageID uint64
	LSN    uint64
	Data   []byte
}

func EncodePageRecord(frame PageRecord) []byte {
	out := make([]byte, 24+len(frame.Data))
	binary.LittleEndian.PutUint64(out[0:8], frame.PageID)
	binary.LittleEndian.PutUint64(out[8:16], frame.LSN)
	binary.LittleEndian.PutUint64(out[16:24], uint64(len(frame.Data)))
	copy(out[24:], frame.Data)
	return out
}

func DecodePageRecord(data []byte) (PageRecord, error) {
	if len(data) < 24 {
		return PageRecord{}, errors.New("truncated page wal record")
	}
	n := int(binary.LittleEndian.Uint64(data[16:24]))
	if n < 0 || n > len(data)-24 {
		return PageRecord{}, errors.New("invalid page wal payload length")
	}
	if len(data) != 24+n {
		return PageRecord{}, errors.New("trailing page wal bytes")
	}
	return PageRecord{
		PageID: binary.LittleEndian.Uint64(data[0:8]),
		LSN:    binary.LittleEndian.Uint64(data[8:16]),
		Data:   append([]byte(nil), data[24:]...),
	}, nil
}
