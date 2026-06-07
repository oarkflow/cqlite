package wal

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestReplayIgnoresPartialTrailingRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.wal")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Type: RecordCommit, TxID: 1, Data: []byte("committed")}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := log.file.Write([]byte{byte(RecordCommit), 2, 0, 0}); err != nil {
		t.Fatal(err)
	}
	var records []Record
	if err := log.Replay(func(rec Record) error {
		records = append(records, rec)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TxID != 1 || string(records[0].Data) != "committed" {
		t.Fatalf("records = %#v", records)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayIgnoresPartialTrailingPayload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.wal")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Type: RecordCommit, TxID: 1, Data: []byte("committed")}, false); err != nil {
		t.Fatal(err)
	}
	var header [21]byte
	header[0] = byte(RecordCommit)
	binary.LittleEndian.PutUint64(header[1:9], 2)
	binary.LittleEndian.PutUint64(header[9:17], 100)
	if _, err := log.file.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := log.file.Write([]byte("torn")); err != nil {
		t.Fatal(err)
	}
	var records []Record
	if err := log.Replay(func(rec Record) error {
		records = append(records, rec)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].TxID != 1 || string(records[0].Data) != "committed" {
		t.Fatalf("records = %#v", records)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRejectsCorruptChecksum(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.wal")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Record{Type: RecordCommit, TxID: 1, Data: []byte("committed")}, false); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	log, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	err = log.Replay(func(Record) error { return nil })
	if err == nil || err.Error() != "wal checksum mismatch" {
		t.Fatalf("Replay err = %v, want wal checksum mismatch", err)
	}
}

func TestReplayRejectsOversizedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.wal")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	var header [21]byte
	header[0] = byte(RecordCommit)
	binary.LittleEndian.PutUint64(header[1:9], 1)
	binary.LittleEndian.PutUint64(header[9:17], uint64(maxRecordDataSize)+1)
	if _, err := log.file.Write(header[:]); err != nil {
		t.Fatal(err)
	}
	err = log.Replay(func(Record) error { return nil })
	if err == nil || err.Error() != "wal record too large" {
		t.Fatalf("Replay err = %v, want wal record too large", err)
	}
}

func FuzzReplay(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{byte(RecordCommit), 1, 0, 0})
	f.Add([]byte("not a wal record"))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		data = append([]byte(nil), data...)
		if len(data) >= 21 {
			binary.LittleEndian.PutUint64(data[9:17], uint64(len(data)-21))
		}
		path := filepath.Join(t.TempDir(), "db.wal")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		log, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer log.Close()
		_ = log.Replay(func(Record) error { return nil })
	})
}

func FuzzAppendReplay(f *testing.F) {
	f.Add(uint8(RecordCommit), uint64(1), []byte("commit"))
	f.Add(uint8(RecordSnapshot), uint64(2), []byte("{}"))

	f.Fuzz(func(t *testing.T, typ uint8, txid uint64, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		path := filepath.Join(t.TempDir(), "db.wal")
		log, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		want := Record{Type: RecordType(typ), TxID: txid, Data: data}
		if err := log.Append(want, false); err != nil {
			t.Fatal(err)
		}
		var got []Record
		if err := log.Replay(func(rec Record) error {
			got = append(got, rec)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0].Type != want.Type || got[0].TxID != want.TxID || string(got[0].Data) != string(want.Data) {
			t.Fatalf("replayed records = %#v, want %#v", got, want)
		}
		if err := log.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPageRecordRoundTrip(t *testing.T) {
	want := PageRecord{PageID: 7, LSN: 99, Data: []byte("page bytes")}
	got, err := DecodePageRecord(EncodePageRecord(want))
	if err != nil {
		t.Fatal(err)
	}
	if got.PageID != want.PageID || got.LSN != want.LSN || string(got.Data) != string(want.Data) {
		t.Fatalf("page record = %#v, want %#v", got, want)
	}
}

func TestPageRecordReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.wal")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	want := PageRecord{PageID: 3, LSN: 10, Data: []byte("payload")}
	if err := log.Append(Record{Type: RecordPage, TxID: 42, Data: EncodePageRecord(want)}, false); err != nil {
		t.Fatal(err)
	}
	var got PageRecord
	if err := log.Replay(func(rec Record) error {
		if rec.Type != RecordPage || rec.TxID != 42 {
			t.Fatalf("record = %#v", rec)
		}
		got, err = DecodePageRecord(rec.Data)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got.PageID != want.PageID || got.LSN != want.LSN || string(got.Data) != string(want.Data) {
		t.Fatalf("page record = %#v, want %#v", got, want)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func FuzzPageRecordDecode(f *testing.F) {
	f.Add(EncodePageRecord(PageRecord{PageID: 1, LSN: 2, Data: []byte("seed")}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			data = data[:8192]
		}
		_, _ = DecodePageRecord(data)
	})
}
