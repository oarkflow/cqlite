package pager

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/sujit/sqlite/internal/page"
)

func TestPagerWriteReadAllocate(t *testing.T) {
	p, err := Open(t.TempDir()+"/pages.db", page.DefaultSize)
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
