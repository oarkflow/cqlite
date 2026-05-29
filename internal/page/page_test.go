package page

import "testing"

func TestEncodeDecodeChecksum(t *testing.T) {
	p := New(7, DefaultSize)
	p.LSN = 42
	copy(p.Data, []byte("hello"))
	buf, err := Encode(p, DefaultSize)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(buf)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 7 || got.LSN != 42 || string(got.Data[:5]) != "hello" {
		t.Fatalf("decoded page mismatch: %+v", got)
	}
	buf[len(buf)-1] ^= 0xff
	if _, err := Decode(buf); err == nil {
		t.Fatal("expected checksum error")
	}
}

func FuzzDecode(f *testing.F) {
	p := New(1, DefaultSize)
	p.LSN = 9
	copy(p.Data, []byte("seed"))
	encoded, err := Encode(p, DefaultSize)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(encoded)
	f.Add([]byte{})
	f.Add([]byte("short"))

	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Decode(data)
	})
}

func FuzzEncodeDecode(f *testing.F) {
	f.Add(uint64(1), uint64(2), []byte("hello"))
	f.Add(uint64(99), uint64(123), []byte{})

	f.Fuzz(func(t *testing.T, id uint64, lsn uint64, payload []byte) {
		maxPayload := DefaultSize - HeaderSize
		if len(payload) > maxPayload {
			payload = payload[:maxPayload]
		}
		p := Page{ID: ID(id), LSN: lsn, Data: append([]byte(nil), payload...)}
		buf, err := Encode(p, DefaultSize)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(buf)
		if err != nil {
			t.Fatal(err)
		}
		if got.ID != p.ID || got.LSN != p.LSN || string(got.Data) != string(p.Data) {
			t.Fatalf("round trip mismatch: got=%+v want=%+v", got, p)
		}
	})
}
