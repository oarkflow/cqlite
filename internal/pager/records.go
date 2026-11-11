package pager

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/sujit/sqlite/internal/page"
)

const (
	recordHeaderMagic = "SQLTREC1"
	recordPageMagic   = "SQLTROW1"
	maxRecordPayload  = 256 << 20
)

type Record struct {
	Key    int64  `json:"key"`
	Values []byte `json:"values"`
}

func (p *Pager) WriteRecords(records []Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pageSize <= page.HeaderSize {
		return errors.New("page size too small")
	}
	pageData := make([]byte, p.pageSize-page.HeaderSize)
	encodedPage := make([]byte, p.pageSize)
	header := page.New(1, p.pageSize)
	copy(header.Data[0:8], []byte(recordHeaderMagic))
	binary.LittleEndian.PutUint64(header.Data[8:16], uint64(len(records)))
	if err := page.EncodeInto(header, p.pageSize, encodedPage); err != nil {
		return err
	}
	if n, err := p.file.WriteAt(encodedPage, 0); err != nil {
		return err
	} else if n != len(encodedPage) {
		return io.ErrShortWrite
	}
	nextPage := page.ID(2)
	for _, rec := range records {
		used, err := p.writeRecordPayload(nextPage, rec, pageData, encodedPage)
		if err != nil {
			return err
		}
		nextPage += page.ID(used)
	}
	if err := p.file.Sync(); err != nil {
		return err
	}
	return syncDirFromFile(p.path)
}

func (p *Pager) ReadRecords() ([]Record, error) {
	pages, err := p.readAllRecordPages()
	if err != nil {
		return nil, err
	}
	if len(pages) == 0 {
		return nil, errors.New("not a record pager")
	}
	header := pages[0]
	if string(header.Data[0:8]) != recordHeaderMagic {
		return nil, errors.New("not a record pager")
	}
	n := int(binary.LittleEndian.Uint64(header.Data[8:16]))
	records := make([]Record, 0, n)
	nextPage := page.ID(2)
	for i := 0; i < n; i++ {
		payload, used, err := readRecordPayloadFromPages(pages, nextPage, p.pageSize)
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

func (p *Pager) ReadRecordCount() (int, error) {
	header, err := p.Read(1)
	if err != nil {
		return 0, err
	}
	if string(header.Data[0:8]) != recordHeaderMagic {
		return 0, errors.New("not a record pager")
	}
	return int(binary.LittleEndian.Uint64(header.Data[8:16])), nil
}

func (p *Pager) ReadRecordAtOrdinal(ord int) (Record, bool, error) {
	if ord < 0 {
		return Record{}, false, nil
	}
	payload, _, err := p.readRecordPayload(page.ID(ord + 2))
	if err != nil {
		return Record{}, false, err
	}
	rec, err := decodeRecord(payload)
	if err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

func (p *Pager) readAllRecordPages() ([]page.Page, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	info, err := p.file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() == 0 {
		return nil, nil
	}
	if info.Size()%int64(p.pageSize) != 0 {
		return nil, errors.New("pager file is not page aligned")
	}
	raw := make([]byte, int(info.Size()))
	if _, err := p.file.ReadAt(raw, 0); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	pages := make([]page.Page, 0, len(raw)/p.pageSize)
	for off := 0; off < len(raw); off += p.pageSize {
		pg, err := page.Decode(raw[off : off+p.pageSize])
		if err != nil {
			return nil, err
		}
		if pg.ID != page.ID(off/p.pageSize+1) {
			return nil, errors.New("page id mismatch")
		}
		pages = append(pages, pg)
	}
	return pages, nil
}

func (p *Pager) writeRecordPayload(start page.ID, rec Record, pageData, encodedPage []byte) (int, error) {
	chunkSize := p.pageSize - page.HeaderSize - 16
	if chunkSize <= 0 {
		return 0, errors.New("page size too small for record")
	}
	recordSize := 12 + len(rec.Values)
	if recordSize > maxRecordPayload {
		return 0, errors.New("record payload too large")
	}
	count := (recordSize + chunkSize - 1) / chunkSize
	if count == 0 {
		count = 1
	}
	var recordHeader [12]byte
	binary.LittleEndian.PutUint64(recordHeader[0:8], uint64(rec.Key))
	binary.LittleEndian.PutUint32(recordHeader[8:12], uint32(len(rec.Values)))
	for i := 0; i < count; i++ {
		begin := i * chunkSize
		end := begin + chunkSize
		if end > recordSize {
			end = recordSize
		}
		clear(pageData)
		copy(pageData[0:8], []byte(recordPageMagic))
		if i == 0 {
			binary.LittleEndian.PutUint64(pageData[8:16], uint64(recordSize))
		}
		copyRecordBytes(pageData[16:], begin, end, recordHeader[:], rec.Values)
		pg := page.Page{ID: start + page.ID(i), Data: pageData}
		if err := page.EncodeInto(pg, p.pageSize, encodedPage); err != nil {
			return 0, err
		}
		n, err := p.file.WriteAt(encodedPage, int64(pg.ID-1)*int64(p.pageSize))
		if err != nil {
			return 0, err
		}
		if n != len(encodedPage) {
			return 0, io.ErrShortWrite
		}
	}
	return count, nil
}

func copyRecordBytes(dst []byte, begin, end int, header []byte, values []byte) {
	if begin < len(header) {
		headerEnd := end
		if headerEnd > len(header) {
			headerEnd = len(header)
		}
		headerBytes := headerEnd - begin
		copy(dst[:headerBytes], header[begin:headerEnd])
		dst = dst[headerBytes:]
		begin = headerEnd
	}
	if begin < end {
		valueBegin := begin - 12
		copy(dst, values[valueBegin:valueBegin+end-begin])
	}
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
	if size < 0 || size > maxRecordPayload {
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
	info, err := p.file.Stat()
	if err != nil {
		return nil, 0, err
	}
	if int64(start+page.ID(count)-1)*int64(p.pageSize) > info.Size() {
		return nil, 0, errors.New("invalid record overflow page")
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

func readRecordPayloadFromPages(pages []page.Page, start page.ID, pageSize int) ([]byte, int, error) {
	firstIdx := int(start) - 1
	if firstIdx < 0 || firstIdx >= len(pages) {
		return nil, 0, errors.New("invalid record page")
	}
	first := pages[firstIdx]
	if string(first.Data[0:8]) != recordPageMagic {
		return nil, 0, errors.New("invalid record page")
	}
	size := int(binary.LittleEndian.Uint64(first.Data[8:16]))
	if size < 0 || size > maxRecordPayload {
		return nil, 0, errors.New("invalid record size")
	}
	chunkSize := pageSize - page.HeaderSize - 16
	if chunkSize <= 0 {
		return nil, 0, errors.New("page size too small for record")
	}
	count := (size + chunkSize - 1) / chunkSize
	if count == 0 {
		count = 1
	}
	if firstIdx+count > len(pages) {
		return nil, 0, errors.New("invalid record overflow page")
	}
	out := make([]byte, 0, size)
	for i := 0; i < count; i++ {
		idx := firstIdx + i
		if idx < 0 || idx >= len(pages) {
			return nil, 0, errors.New("invalid record overflow page")
		}
		pg := pages[idx]
		if i > 0 && string(pg.Data[0:8]) != recordPageMagic {
			return nil, 0, errors.New("invalid record overflow page")
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
