package pager

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"

	"github.com/sujit/sqlite/internal/page"
)

func TestPagerWriteReadAllocate(t *testing.T) {
	path := t.TempDir() + "/pages.db"
	p, err := Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	id, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	pg := page.New(id, page.DefaultSize)
	copy(pg.Data, []byte("pager"))
	if err := p.Write(pg); err != nil {
		t.Fatal(err)
	}
	if err := p.Sync(); err != nil {
		t.Fatal(err)
	}
	got, err := p.Read(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Data[:5], []byte("pager")) {
		t.Fatalf("payload = %q", got.Data[:5])
	}
	if err := p.Free(id); err != nil {
		t.Fatal(err)
	}
	reused, err := p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if reused != id {
		t.Fatalf("reused page = %d, want %d", reused, id)
	}
	if err := p.Free(reused); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	reused, err = p.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	if reused != id {
		t.Fatalf("persisted reused page = %d, want %d", reused, id)
	}
}

func TestPagerRejectsPageIDMismatch(t *testing.T) {
	path := t.TempDir() + "/pages.db"
	p, err := Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	pg := page.New(1, page.DefaultSize)
	copy(pg.Data, []byte("pager"))
	if err := p.Write(pg); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(raw[0:8], 99)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.Read(1); err == nil {
		t.Fatal("expected page id mismatch")
	}
}

func TestPagerRecordsRoundTrip(t *testing.T) {
	p, err := Open(t.TempDir()+"/records.db", page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	payload, _ := json.Marshal(map[string]any{"name": "Ada"})
	records := []Record{{Key: 7, Values: payload}}
	if err := p.WriteRecords(records); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != 7 || !bytes.Equal(got[0].Values, payload) {
		t.Fatalf("records = %#v", got)
	}
}

func TestPagerRecordsRoundTripBinaryPayload(t *testing.T) {
	p, err := Open(t.TempDir()+"/records.db", page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	payload := []byte{0, 1, 2, '{', 0xff}
	records := []Record{{Key: -7, Values: payload}}
	if err := p.WriteRecords(records); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Key != -7 || !bytes.Equal(got[0].Values, payload) {
		t.Fatalf("records = %#v", got)
	}
}

func TestPagerRecordsOverflowRoundTrip(t *testing.T) {
	p, err := Open(t.TempDir()+"/records.db", 128)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	payload := bytes.Repeat([]byte("abcdef"), 100)
	records := []Record{
		{Key: 1, Values: []byte("small")},
		{Key: 2, Values: payload},
		{Key: 3, Values: []byte("after")},
	}
	if err := p.WriteRecords(records); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(records) {
		t.Fatalf("record count = %d, want %d", len(got), len(records))
	}
	for i := range records {
		if got[i].Key != records[i].Key || !bytes.Equal(got[i].Values, records[i].Values) {
			t.Fatalf("record %d = %#v, want %#v", i, got[i], records[i])
		}
	}
}

func TestPagerReadsLegacyJSONRecords(t *testing.T) {
	payload, _ := json.Marshal(Record{Key: 7, Values: []byte("legacy")})
	rec, err := decodeRecord(payload)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Key != 7 || !bytes.Equal(rec.Values, []byte("legacy")) {
		t.Fatalf("legacy record = %#v", rec)
	}
}

func TestPagerBlobRoundTrip(t *testing.T) {
	p, err := Open(t.TempDir()+"/blob.db", 128)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	data := bytes.Repeat([]byte("abcdef"), 100)
	if err := p.WriteBlob(data); err != nil {
		t.Fatal(err)
	}
	got, err := p.ReadBlob()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("blob mismatch: got %d bytes want %d", len(got), len(data))
	}
}

func TestPagerRejectsCorruptBlobHeader(t *testing.T) {
	path := t.TempDir() + "/blob.db"
	p, err := Open(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.WriteBlob([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[page.HeaderSize] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = Open(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ReadBlob(); err == nil {
		t.Fatal("expected corrupt blob header to fail")
	}
}

func TestPagerRejectsCorruptRecordSize(t *testing.T) {
	path := t.TempDir() + "/records.db"
	p, err := Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"name": "Ada"})
	if err := p.WriteRecords([]Record{{Key: 1, Values: payload}}); err != nil {
		t.Fatal(err)
	}
	pg, err := p.Read(2)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(pg.Data[8:16], uint64(len(pg.Data)))
	if err := p.Write(pg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadRecords(); err == nil {
		t.Fatal("expected corrupt record size to fail")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPagerRejectsOversizedRecordPayload(t *testing.T) {
	path := t.TempDir() + "/records.db"
	p, err := Open(path, page.DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"name": "Ada"})
	if err := p.WriteRecords([]Record{{Key: 1, Values: payload}}); err != nil {
		t.Fatal(err)
	}
	pg, err := p.Read(2)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint64(pg.Data[8:16], uint64(maxRecordPayload)+1)
	if err := p.Write(pg); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ReadRecords(); err == nil {
		t.Fatal("expected oversized record payload to fail")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPagerRejectsCorruptRecordOverflowPage(t *testing.T) {
	path := t.TempDir() + "/records.db"
	p, err := Open(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("abcdef"), 100)
	if err := p.WriteRecords([]Record{{Key: 1, Values: payload}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 3*128 {
		t.Fatalf("expected overflow pages, got %d bytes", len(raw))
	}
	raw[2*128+page.HeaderSize] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	p, err = Open(path, 128)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.ReadRecords(); err == nil {
		t.Fatal("expected corrupt overflow page to fail")
	}
}
