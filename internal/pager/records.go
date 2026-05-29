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
	Key    int64           `json:"key"`
	Values json.RawMessage `json:"values"`
}

func (p *Pager) WriteRecords(records []Record) error {
	header := page.New(1, p.pageSize)
	copy(header.Data[0:8], []byte(recordHeaderMagic))
	binary.LittleEndian.PutUint64(header.Data[8:16], uint64(len(records)))
	if err := p.Write(header); err != nil {
		return err
	}
	for i, rec := range records {
		data, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		if len(data) > p.pageSize-page.HeaderSize-16 {
			return errors.New("record too large for page")
		}
		pg := page.New(page.ID(i+2), p.pageSize)
		copy(pg.Data[0:8], []byte(recordPageMagic))
		binary.LittleEndian.PutUint64(pg.Data[8:16], uint64(len(data)))
		copy(pg.Data[16:], data)
		if err := p.Write(pg); err != nil {
			return err
		}
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
	for i := 0; i < n; i++ {
		pg, err := p.Read(page.ID(i + 2))
		if err != nil {
			return nil, err
		}
		if string(pg.Data[0:8]) != recordPageMagic {
			return nil, errors.New("invalid record page")
		}
		size := int(binary.LittleEndian.Uint64(pg.Data[8:16]))
		if size < 0 || size > len(pg.Data)-16 {
			return nil, errors.New("invalid record size")
		}
		var rec Record
		if err := json.Unmarshal(pg.Data[16:16+size], &rec); err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, nil
}
