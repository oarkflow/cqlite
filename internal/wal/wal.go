package wal

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"hash/crc32"
	"io"
	"os"
	"sync"
	"syscall"
)

var crcTable = crc32.MakeTable(crc32.IEEE)

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

type Log struct {
	mu   sync.Mutex
	path string
	file *os.File
}

func Open(path string) (*Log, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &Log{path: path, file: f}, nil
}

func (l *Log) Append(rec Record, sync bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var header [21]byte
	header[0] = byte(rec.Type)
	binary.LittleEndian.PutUint64(header[1:9], rec.TxID)
	binary.LittleEndian.PutUint64(header[9:17], uint64(len(rec.Data)))
	sum := crc32.Update(0, crcTable, header[:17])
	sum = crc32.Update(sum, crcTable, rec.Data)
	binary.LittleEndian.PutUint32(header[17:21], sum)
	if err := l.writeRecord(header[:], rec.Data); err != nil {
		return err
	}
	if sync {
		return syncFile(l.file)
	}
	return nil
}

func syncFile(f *os.File) error {
	for {
		err := syscall.Fsync(int(f.Fd()))
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
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := l.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r := bufio.NewReader(l.file)
	for {
		header := make([]byte, 21)
		if _, err := io.ReadFull(r, header); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return nil
			}
			return err
		}
		n := binary.LittleEndian.Uint64(header[9:17])
		if n > 1<<32 {
			return errors.New("wal record too large")
		}
		data := make([]byte, n)
		if _, err := io.ReadFull(r, data); err != nil {
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
	}
}

func (l *Log) Checkpoint(snapshot any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.path + ".snapshot.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path+".snapshot")
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file.Close()
}
