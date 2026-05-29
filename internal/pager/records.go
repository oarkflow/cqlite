package pager

import (
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/sujit/sqlite/internal/page"
)

const (
	recordHeaderMagic = "SQLTREC1"
	recordPageMagic   = "SQLTROW1"
)

type Record struct {
	Key    int64  `json:"key"`
	Values []byte `json:"values"`
}

func (p *Pager) WriteRecords(records []Record) error {
	header := page.New(1, p.pageSize)
	copy(header.Data[0:8], []byte(recordHeaderMagic))
	binary.LittleEndian.PutUint64(header.Data[8:16], uint64(len(records)))
	if err := p.Write(header); err != nil {
		return err
	}
	nextPage := page.ID(2)
	for _, rec := range records {
		data := encodeRecord(rec)
		used, err := p.writeRecordPayload(nextPage, data)
		if err != nil {
			return err
		}
		nextPage += page.ID(used)
	}
	return p.Sync()
}

func (p *Pager) ReadRecords() ([]Record, error) {
	header, err := p.Read(1)
	if err != nil {
		return nil, err
	}
	if string(header.Data[0:8]) != recordHeaderMagic {
		return nil, errors.New("not a record pager")
	}
	n := int(binary.LittleEndian.Uint64(header.Data[8:16]))
	records := make([]Record, 0, n)
	nextPage := page.ID(2)
	for i := 0; i < n; i++ {
		payload, used, err := p.readRecordPayload(nextPage)
		if err != nil {
			return nil, err
		}
		rec, err := decodeRecord(payload)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
		nextPage += page.ID(used)
	}
	return records, nil
}

func (p *Pager) writeRecordPayload(start page.ID, data []byte) (int, error) {
	chunkSize := p.pageSize - page.HeaderSize - 16
	if chunkSize <= 0 {
		return 0, errors.New("page size too small for record")
	}
	count := (len(data) + chunkSize - 1) / chunkSize
	if count == 0 {
		count = 1
	}
	for i := 0; i < count; i++ {
		begin := i * chunkSize
		end := begin + chunkSize
		if end > len(data) {
			end = len(data)
		}
		pg := page.New(start+page.ID(i), p.pageSize)
		copy(pg.Data[0:8], []byte(recordPageMagic))
		if i == 0 {
			binary.LittleEndian.PutUint64(pg.Data[8:16], uint64(len(data)))
		}
		copy(pg.Data[16:], data[begin:end])
		if err := p.Write(pg); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func (p *Pager) readRecordPayload(start page.ID) ([]byte, int, error) {
	first, err := p.Read(start)
	if err != nil {
		return nil, 0, err
	}
	if string(first.Data[0:8]) != recordPageMagic {
		return nil, 0, errors.New("invalid record page")
	}
	size := int(binary.LittleEndian.Uint64(first.Data[8:16]))
	if size < 0 {
		return nil, 0, errors.New("invalid record size")
	}
	chunkSize := p.pageSize - page.HeaderSize - 16
	if chunkSize <= 0 {
		return nil, 0, errors.New("page size too small for record")
	}
	count := (size + chunkSize - 1) / chunkSize
	if count == 0 {
		count = 1
	}
	out := make([]byte, 0, size)
	for i := 0; i < count; i++ {
		pg := first
		if i > 0 {
			pg, err = p.Read(start + page.ID(i))
			if err != nil {
				return nil, 0, err
			}
			if string(pg.Data[0:8]) != recordPageMagic {
				return nil, 0, errors.New("invalid record overflow page")
			}
		}
		remaining := size - len(out)
		n := chunkSize
		if remaining < n {
			n = remaining
		}
		out = append(out, pg.Data[16:16+n]...)
	}
	return out, count, nil
}

func encodeRecord(rec Record) []byte {
	out := make([]byte, 12+len(rec.Values))
	binary.LittleEndian.PutUint64(out[0:8], uint64(rec.Key))
	binary.LittleEndian.PutUint32(out[8:12], uint32(len(rec.Values)))
	copy(out[12:], rec.Values)
	return out
}

func decodeRecord(data []byte) (Record, error) {
	if len(data) > 0 && data[0] == '{' {
		var rec Record
		if err := json.Unmarshal(data, &rec); err != nil {
			return Record{}, err
		}
		return rec, nil
	}
	if len(data) < 12 {
		return Record{}, errors.New("truncated record payload")
	}
	n := int(binary.LittleEndian.Uint32(data[8:12]))
	if n < 0 || n > len(data)-12 {
		return Record{}, errors.New("invalid record payload length")
	}
	return Record{
		Key:    int64(binary.LittleEndian.Uint64(data[0:8])),
		Values: append([]byte(nil), data[12:12+n]...),
	}, nil
}
