package cqlite

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"math"
	"testing"
	"time"
)

type marshaledTextValue struct{ value string }

func (v *marshaledTextValue) MarshalText() ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return []byte(v.value), nil
}

type failingTextValue struct{}

func (failingTextValue) MarshalText() ([]byte, error) { return nil, context.DeadlineExceeded }

type recursiveSQLValuer struct{}

func (recursiveSQLValuer) Value() (driver.Value, error) { return recursiveSQLValuer{}, nil }

func TestTextMarshalerBindingsUseSQLiteTextStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE text_values (id INTEGER PRIMARY KEY, value ANY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO text_values VALUES (1, ?)`, &marshaledTextValue{value: "uuid:00112233"}); err != nil {
		t.Fatal(err)
	}
	var nilText *marshaledTextValue
	if _, err := d.Exec(ctx, `INSERT INTO text_values VALUES (2, ?)`, nilText); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO text_values VALUES (3, ?)`, failingTextValue{}); err == nil {
		t.Fatal("expected TextMarshaler error to propagate")
	}
	if _, err := d.Exec(ctx, `INSERT INTO text_values VALUES (4, ?)`, recursiveSQLValuer{}); err == nil {
		t.Fatal("expected invalid recursive driver.Valuer result to return an error")
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(ctx, `SELECT value, typeof(value) FROM text_values ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != "uuid:00112233" || row[1] != "text" {
		t.Fatalf("text value = %#v, ok=%v; want TEXT storage", row, ok)
	}
	if row, ok := rows.Next(); !ok || row[0] != nil || row[1] != "null" {
		t.Fatalf("nil TextMarshaler value = %#v, ok=%v; want NULL", row, ok)
	}
}

func TestCommonGoValuesUseSQLiteStorageClasses(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE scalar_types (
		id INTEGER PRIMARY KEY, small INTEGER, unsigned INTEGER, fraction REAL,
		flag INTEGER, stamp TEXT, any_value ANY, payload BLOB, optional ANY, null_value ANY
	)`); err != nil {
		t.Fatal(err)
	}
	payload := []byte{0x12, 0x34, 0x56}
	stamp := time.Date(2026, time.November, 11, 12, 30, 45, 123456789, time.UTC)
	if _, err := d.Exec(ctx,
		`INSERT INTO scalar_types VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		int32(1), int8(-8), uint16(60000), float32(1.25), true, stamp,
		int32(42), payload, sql.NullString{String: "ready", Valid: true}, sql.NullString{},
	); err != nil {
		t.Fatal(err)
	}
	payload[0] = 0xff // SQLite bindings must not retain the caller's byte slice.
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	d, err = Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(ctx, `SELECT small, unsigned, fraction, flag, stamp, any_value, payload, optional, null_value FROM scalar_types WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok {
		t.Fatal("missing scalar type row")
	}
	if row[0] != int64(-8) || row[1] != int64(60000) || row[2] != float64(1.25) || row[3] != int64(1) {
		t.Fatalf("numeric storage values = %#v", row[:4])
	}
	if row[4] != stamp.Format(time.RFC3339Nano) || row[5] != int64(42) {
		t.Fatalf("text/any storage values = %#v", row[4:6])
	}
	if got, ok := row[6].([]byte); !ok || !bytes.Equal(got, []byte{0x12, 0x34, 0x56}) {
		t.Fatalf("blob storage value = %#v", row[6])
	}
	if row[7] != "ready" || row[8] != nil {
		t.Fatalf("sql.NullString values = %#v, want ready and NULL", row[7:9])
	}
	if _, err := d.Exec(ctx, `INSERT INTO scalar_types(id, unsigned) VALUES (2, ?)`, ^uint64(0)); err == nil {
		t.Fatal("expected oversized uint64 to be rejected")
	}
}

func TestStructuredAndJSONNumberBindingsKeepStorageClassAfterReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE structured_values (id INTEGER PRIMARY KEY, value ANY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO structured_values VALUES (1, ?)`, json.Number("42")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO structured_values VALUES (2, :value)`, Named("value", map[string]any{"answer": int64(42)})); err != nil {
		t.Fatal(err)
	}
	assert := func(db *DB) {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT value FROM structured_values ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		if row, ok := rows.Next(); !ok || row[0] != "42" {
			t.Fatalf("json.Number storage value = %#v, ok=%v; want text 42", row, ok)
		}
		if row, ok := rows.Next(); !ok || row[0] != `{"answer":42}` {
			t.Fatalf("structured storage value = %#v, ok=%v; want JSON text", row, ok)
		}
	}
	assert(d)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	assert(d)
}

func TestJSONColumnsParseValidateAndPersistDocuments(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE json_values (id INTEGER PRIMARY KEY, document JSON)`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO json_values VALUES (:id, :document)`, Named("id", 1), Named("document", map[string]any{"answer": 42})); err != nil {
		t.Fatalf("insert structured JSON: %v", err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO json_values VALUES (:id, :document)`, Named("id", 2), Named("document", `{"items":[true,null]}`)); err != nil {
		t.Fatalf("insert JSON text: %v", err)
	}
	if _, err := d.Exec(ctx, `INSERT INTO json_values VALUES (:id, :document)`, Named("id", 3), Named("document", `{"broken":}`)); err == nil {
		t.Fatal("expected malformed JSON document to be rejected")
	}
	info, err := d.Query(ctx, `PRAGMA table_info(json_values)`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := info.Next(); !ok || row[1] != "id" {
		t.Fatalf("first table_info row = %#v, ok=%v", row, ok)
	}
	if row, ok := info.Next(); !ok || row[2] != "JSON" {
		t.Fatalf("JSON declared type = %#v, ok=%v; want JSON", row, ok)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(ctx, `SELECT json_extract(document, '$.answer'), json_valid(document) FROM json_values WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(42) || row[1] != int64(1) {
		t.Fatalf("reopened JSON values = %#v, ok=%v; want 42 and valid", row, ok)
	}
	if _, err := d.Exec(ctx, `UPDATE json_values SET document = json_set(document, '$.answer', 43) WHERE json_extract(document, '$.answer') = 42`); err != nil {
		t.Fatalf("update JSON value: %v", err)
	}
	rows, err = d.Query(ctx, `SELECT json_extract(document, '$.answer') FROM json_values WHERE id = 1`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(43) {
		t.Fatalf("updated JSON query = %#v, ok=%v; want 43", row, ok)
	}
	if _, err := d.Exec(ctx, `DELETE FROM json_values WHERE json_extract(document, '$.answer') = 43`); err != nil {
		t.Fatalf("delete by JSON value: %v", err)
	}
	rows, err = d.Query(ctx, `SELECT id FROM json_values ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	if row, ok := rows.Next(); !ok || row[0] != int64(2) {
		t.Fatalf("remaining rows after JSON delete = %#v, ok=%v; want id 2", row, ok)
	}
	if _, ok := rows.Next(); ok {
		t.Fatal("JSON delete left an unexpected row")
	}
}

func TestEmptyBlobRemainsDistinctFromNullAcrossHandlesAndRecovery(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	first, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `CREATE TABLE blob_values (id INTEGER PRIMARY KEY, value BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `INSERT INTO blob_values VALUES (1, ?)`, []byte{}); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Exec(ctx, `INSERT INTO blob_values VALUES (2, ?)`, []byte(nil)); err != nil {
		t.Fatal(err)
	}
	second, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	assert := func(d *DB) {
		t.Helper()
		rows, err := d.Query(ctx, `SELECT value FROM blob_values ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		blobRow, ok := rows.Next()
		blob, isBlob := blobRow[0].([]byte)
		if !ok || !isBlob || blob == nil || len(blob) != 0 {
			t.Fatalf("empty BLOB = %#v, ok=%v; want non-nil empty []byte", blobRow, ok)
		}
		nullRow, ok := rows.Next()
		if !ok || nullRow[0] != nil {
			t.Fatalf("NULL = %#v, ok=%v; want nil", nullRow, ok)
		}
	}
	assert(second)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	assert(reopened)
}

func TestNaNAndInfinityBindingsMatchSQLiteStorage(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	d, err := Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(ctx, `CREATE TABLE float_values (id INTEGER PRIMARY KEY, value ANY)`); err != nil {
		t.Fatal(err)
	}
	values := []any{math.NaN(), math.Inf(1), math.Inf(-1)}
	for i, value := range values {
		if _, err := d.Exec(ctx, `INSERT INTO float_values VALUES (?, ?)`, int64(i+1), value); err != nil {
			t.Fatal(err)
		}
	}
	assert := func(db *DB) {
		t.Helper()
		rows, err := db.Query(ctx, `SELECT value FROM float_values ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		if row, ok := rows.Next(); !ok || row[0] != nil {
			t.Fatalf("NaN storage value = %#v, ok=%v; want NULL", row, ok)
		}
		for _, want := range []float64{math.Inf(1), math.Inf(-1)} {
			row, ok := rows.Next()
			got, isFloat := row[0].(float64)
			if !ok || !isFloat || got != want {
				t.Fatalf("infinity storage value = %#v, ok=%v; want %v", row, ok, want)
			}
		}
	}
	assert(d)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(dir, Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	assert(d)
}

func TestNaNExpressionResultsMatchSQLite(t *testing.T) {
	d, err := Open(t.TempDir(), Options{SyncPolicy: SyncOff})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Query(context.Background(), `SELECT sqrt(-1), typeof(sqrt(-1)), (1e308 * 1e308) - (1e308 * 1e308)`)
	if err != nil {
		t.Fatal(err)
	}
	row, ok := rows.Next()
	if !ok || row[0] != nil || row[1] != "null" || row[2] != nil {
		t.Fatalf("NaN expression results = %#v, ok=%v; want NULL, null, NULL", row, ok)
	}
}
