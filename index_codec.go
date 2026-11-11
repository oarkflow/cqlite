package cqlite

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
)

var indexRecordMagic = []byte("SQLTIDX1")

type indexRecord struct {
	RowID int64
	Key   []any
}

func encodeIndexRecord(rowid int64, key []any) ([]byte, error) {
	if len(key) > math.MaxUint16 {
		return nil, errors.New("index key has too many columns")
	}
	out := make([]byte, 0, len(indexRecordMagic)+10+len(key)*16)
	out = append(out, indexRecordMagic...)
	var buf [10]byte
	binary.LittleEndian.PutUint64(buf[:8], uint64(rowid))
	binary.LittleEndian.PutUint16(buf[8:10], uint16(len(key)))
	out = append(out, buf[:]...)
	for _, value := range key {
		var ok bool
		out, ok = appendWALValue(out, value)
		if !ok {
			return nil, errors.New("unsupported index key value")
		}
	}
	return out, nil
}

func decodeIndexRecord(data []byte) (indexRecord, error) {
	if len(data) >= len(indexRecordMagic) && string(data[:len(indexRecordMagic)]) == string(indexRecordMagic) {
		data = data[len(indexRecordMagic):]
		if len(data) < 10 {
			return indexRecord{}, errors.New("truncated index record")
		}
		rec := indexRecord{
			RowID: int64(binary.LittleEndian.Uint64(data[:8])),
			Key:   make([]any, 0, binary.LittleEndian.Uint16(data[8:10])),
		}
		data = data[10:]
		for cap(rec.Key) > len(rec.Key) {
			value, rest, err := readWALValue(data)
			if err != nil {
				return indexRecord{}, err
			}
			rec.Key = append(rec.Key, value)
			data = rest
		}
		if len(data) != 0 {
			return indexRecord{}, errors.New("trailing index record bytes")
		}
		return rec, nil
	}
	var legacy struct {
		RowID int64 `json:"rowid"`
		Key   []any `json:"key"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return indexRecord{}, err
	}
	return indexRecord{RowID: legacy.RowID, Key: legacy.Key}, nil
}
