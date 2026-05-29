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
