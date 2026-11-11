package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
)

var crcTable = crc32.MakeTable(crc32.IEEE)

const maxRecordDataSize = 256 << 20

type RecordType uint8

const (
	RecordSnapshot RecordType = 1
	RecordCommit   RecordType = 2
	RecordPage     RecordType = 3
)

type Record struct {
	Type RecordType
	TxID uint64
	Data []byte
}

// RecordSize returns the encoded byte length of a WAL record.
func RecordSize(rec Record) int64 {
	return int64(21 + len(rec.Data))
}

type Log struct {
	mu             sync.Mutex
	path           string
	file           *os.File
	appendHeader   [21]byte
	writeBuf       []byte
	written        int64
	incompleteTail bool
	replayedSize   int64
}

func Open(path string) (*Log, error) {
	_, statErr := os.Stat(path)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	if errors.Is(statErr, os.ErrNotExist) {
		if err := syncDir(filepath.Dir(path)); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return &Log{path: path, file: f}, nil
}

func (l *Log) Written() int64 {
	return atomic.LoadInt64(&l.written)
}

// Size returns the current on-disk WAL size, including records appended by
// other processes that share the database.
func (l *Log) Size() (int64, error) {
	return fileSize(l.file)
}

var ieeeTable *crc32.Table

func init() {
	ieeeTable = crc32.MakeTable(crc32.IEEE)
}

func crc32IEEE(data []byte) uint32 {
	crc := uint32(0xFFFFFFFF)
	for _, b := range data {
		crc = ieeeTable[byte(crc)^b] ^ (crc >> 8)
	}
	return ^crc
}

func (l *Log) Append(rec Record, sync bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	header := l.appendHeader[:]
	header[0] = byte(rec.Type)
	binary.LittleEndian.PutUint64(header[1:9], rec.TxID)
	binary.LittleEndian.PutUint64(header[9:17], uint64(len(rec.Data)))
	sum := crc32.Update(0, ieeeTable, header[:17])
	sum = crc32.Update(sum, ieeeTable, rec.Data)
	binary.LittleEndian.PutUint32(header[17:21], sum)
	total := 21 + len(rec.Data)
	if err := l.writeRecord(header[:], rec.Data); err != nil {
		return err
	}
	atomic.AddInt64(&l.written, int64(total))
	if sync {
		return syncFile(l.file)
	}
	return nil
}

func syncFile(f *os.File) error {
	for {
		err := syncWALData(f)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

func (l *Log) writeRecord(header, data []byte) error {
	total := len(header) + len(data)
	var stack [4096]byte
	var buf []byte
	if total <= len(stack) {
		buf = stack[:total]
	} else if total <= 64<<10 {
		if cap(l.writeBuf) < total {
			l.writeBuf = make([]byte, total)
		}
		buf = l.writeBuf[:total]
	} else {
		buf = make([]byte, total)
	}
	copy(buf, header)
	copy(buf[len(header):], data)
	for len(buf) > 0 {
		n, err := l.file.Write(buf)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		buf = buf[n:]
	}
	return nil
}

func (l *Log) Replay(fn func(Record) error) error {
	return l.replay(fn, false)
}

func (l *Log) HasIncompleteTail() bool { l.mu.Lock(); defer l.mu.Unlock(); return l.incompleteTail }

func (l *Log) ReplaySize() int64 { l.mu.Lock(); defer l.mu.Unlock(); return l.replayedSize }

// ReplayAndRepair replays records and removes an incomplete trailing record.
// Callers must hold the database writer lock before using it.
func (l *Log) ReplayAndRepair(fn func(Record) error) error {
	return l.replay(fn, true)
}

func (l *Log) replay(fn func(Record) error, repair bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	l.replayedSize = info.Size()
	l.incompleteTail = false
	r := bufio.NewReader(io.LimitReader(l.file, l.replayedSize))
	validOffset := int64(0)
	for {
		header := make([]byte, 21)
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, io.ErrUnexpectedEOF) {
				l.incompleteTail = true
				if repair {
					if err := l.truncateIncompleteTail(validOffset); err != nil {
						return err
					}
					l.replayedSize = validOffset
					return nil
				}
				return nil
			}
			return err
		}
		n := binary.LittleEndian.Uint64(header[9:17])
		if n > maxRecordDataSize {
			return errors.New("wal record too large")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				l.incompleteTail = true
				if repair {
					if err := l.truncateIncompleteTail(validOffset); err != nil {
						return err
					}
					l.replayedSize = validOffset
					return nil
				}
				return nil
			}
			return err
		}
		want := binary.LittleEndian.Uint32(header[17:21])
		got := crc32.Update(0, crcTable, header[:17])
		got = crc32.Update(got, crcTable, data)
		if got != want {
			return errors.New("wal checksum mismatch")
		}
		if err := fn(Record{Type: RecordType(header[0]), TxID: binary.LittleEndian.Uint64(header[1:9]), Data: data}); err != nil {
			return err
		}
		validOffset += int64(len(header) + len(data))
	}
}

func (l *Log) truncateIncompleteTail(validOffset int64) error {
	if err := l.file.Truncate(validOffset); err != nil {
		return err
	}
	_, err := l.file.Seek(validOffset, io.SeekStart)
	return err
}

func (l *Log) Checkpoint(snapshot any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".snapshot.tmp"
	if err := writeFileSynced(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.path+".snapshot"); err != nil {
		return err
	}
	return syncDir(filepath.Dir(l.path))
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.file.Close()
	l.writeBuf = nil
	return err
}

func writeFileSynced(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if n, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	} else if n != len(data) {
		_ = f.Close()
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
