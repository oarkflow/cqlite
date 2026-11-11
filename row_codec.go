package db

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"

	"github.com/oarkflow/sqlite/internal/catalog"
)

const (
	rowCodecMagic   = "SQLTROW2"
	rowCodecVersion = 1

	rowValueNull byte = iota
	rowValueInteger
	rowValueReal
	rowValueText
	rowValueBlob
)

func encodeRowVersion(schema *catalog.Table, row rowVersion) ([]byte, error) {
	if schema == nil {
		return nil, errors.New("missing table schema")
	}
	var buf bytes.Buffer
	buf.WriteString(rowCodecMagic)
	buf.WriteByte(rowCodecVersion)
	var fixed [28]byte
	binary.LittleEndian.PutUint64(fixed[0:8], uint64(row.RowID))
	binary.LittleEndian.PutUint64(fixed[8:16], row.Created)
	binary.LittleEndian.PutUint64(fixed[16:24], row.Deleted)
	binary.LittleEndian.PutUint32(fixed[24:28], uint32(len(schema.Columns)))
	buf.Write(fixed[:])
	row.ensurePositioned(schema)
	values := row.positionedValues()
	for i := range schema.Columns {
		if err := encodeRowValue(&buf, values[i]); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func decodeRowVersion(schema *catalog.Table, data []byte) (rowVersion, error) {
	return decodeRowVersionWithOptions(schema, data, true)
}

func decodeRowVersionLazy(schema *catalog.Table, data []byte) (rowVersion, error) {
	return decodeRowVersionWithOptions(schema, data, false)
}

func decodeRowVersionWithOptions(schema *catalog.Table, data []byte, includeValues bool) (rowVersion, error) {
	if schema == nil {
		return rowVersion{}, errors.New("missing table schema")
	}
	if !bytes.HasPrefix(data, []byte(rowCodecMagic)) {
		var row rowVersion
		err := json.Unmarshal(data, &row)
		return row, err
	}
	pos := len(rowCodecMagic)
	if pos >= len(data) || data[pos] != rowCodecVersion {
		return rowVersion{}, errors.New("unsupported row codec version")
	}
	pos++
	if len(data)-pos < 28 {
		return rowVersion{}, errors.New("truncated row header")
	}
	row := rowVersion{
		RowID:   int64(binary.LittleEndian.Uint64(data[pos : pos+8])),
		Created: binary.LittleEndian.Uint64(data[pos+8 : pos+16]),
		Deleted: binary.LittleEndian.Uint64(data[pos+16 : pos+24]),
		Pos:     make([]any, len(schema.Columns)),
	}
	count := int(binary.LittleEndian.Uint32(data[pos+24 : pos+28]))
	pos += 28
	if count != len(schema.Columns) {
		return rowVersion{}, errors.New("row column count does not match schema")
	}
	for i := range schema.Columns {
		value, next, err := decodeRowValue(data, pos)
		if err != nil {
			return rowVersion{}, err
		}
		row.Pos[i] = value
		pos = next
	}
	if pos != len(data) {
		return rowVersion{}, errors.New("trailing row bytes")
	}
	if includeValues {
		row.ensureValues(schema)
	}
	return row, nil
}

func encodeRowValue(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteByte(rowValueNull)
	case int:
		writeRowInt(buf, int64(v))
	case int8:
		writeRowInt(buf, int64(v))
	case int16:
		writeRowInt(buf, int64(v))
	case int32:
		writeRowInt(buf, int64(v))
	case int64:
		writeRowInt(buf, v)
	case uint:
		if uint64(v) > math.MaxInt64 {
			return errors.New("unsigned integer exceeds SQLite INTEGER range")
		}
		writeRowInt(buf, int64(v))
	case uint8:
		writeRowInt(buf, int64(v))
	case uint16:
		writeRowInt(buf, int64(v))
	case uint32:
		writeRowInt(buf, int64(v))
	case uint64:
		if v > math.MaxInt64 {
			return errors.New("unsigned integer exceeds SQLite INTEGER range")
		}
		writeRowInt(buf, int64(v))
	case json.Number:
		if i, err := v.Int64(); err == nil {
			writeRowInt(buf, i)
			return nil
		}
		f, err := v.Float64()
		if err != nil {
			return err
		}
		writeRowFloat(buf, f)
	case float64:
		writeRowFloat(buf, v)
	case float32:
		writeRowFloat(buf, float64(v))
	case bool:
		if v {
			writeRowInt(buf, 1)
		} else {
			writeRowInt(buf, 0)
		}
	case string:
		writeRowBytes(buf, rowValueText, []byte(v))
	case []byte:
		if v == nil {
			buf.WriteByte(rowValueNull)
			break
		}
		writeRowBytes(buf, rowValueBlob, v)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeRowBytes(buf, rowValueText, encoded)
	}
	return nil
}

func writeRowInt(buf *bytes.Buffer, v int64) {
	var tmp [9]byte
	tmp[0] = rowValueInteger
	binary.LittleEndian.PutUint64(tmp[1:9], uint64(v))
	buf.Write(tmp[:])
}

func writeRowFloat(buf *bytes.Buffer, v float64) {
	var tmp [9]byte
	tmp[0] = rowValueReal
	binary.LittleEndian.PutUint64(tmp[1:9], math.Float64bits(v))
	buf.Write(tmp[:])
}

func writeRowBytes(buf *bytes.Buffer, typ byte, data []byte) {
	var tmp [5]byte
	tmp[0] = typ
	binary.LittleEndian.PutUint32(tmp[1:5], uint32(len(data)))
	buf.Write(tmp[:])
	buf.Write(data)
}

func cloneSQLiteBlob(data []byte) []byte {
	if data == nil {
		return nil
	}
	cloned := make([]byte, len(data))
	copy(cloned, data)
	return cloned
}

func decodeRowValue(data []byte, pos int) (any, int, error) {
	if pos >= len(data) {
		return nil, pos, errors.New("truncated row value")
	}
	typ := data[pos]
	pos++
	switch typ {
	case rowValueNull:
		return nil, pos, nil
	case rowValueInteger:
		if len(data)-pos < 8 {
			return nil, pos, errors.New("truncated integer row value")
		}
		return int64(binary.LittleEndian.Uint64(data[pos : pos+8])), pos + 8, nil
	case rowValueReal:
		if len(data)-pos < 8 {
			return nil, pos, errors.New("truncated real row value")
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(data[pos : pos+8])), pos + 8, nil
	case rowValueText, rowValueBlob:
		if len(data)-pos < 4 {
			return nil, pos, errors.New("truncated variable row value")
		}
		n := int(binary.LittleEndian.Uint32(data[pos : pos+4]))
		pos += 4
		if n < 0 || n > len(data)-pos {
			return nil, pos, errors.New("invalid variable row value length")
		}
		if typ == rowValueBlob {
			return cloneSQLiteBlob(data[pos : pos+n]), pos + n, nil
		}
		return string(data[pos : pos+n]), pos + n, nil
	default:
		return nil, pos, errors.New("unknown row value type")
	}
}
