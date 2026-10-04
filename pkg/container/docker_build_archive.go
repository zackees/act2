//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Check framing before archive/tar can allocate extended-header bodies. A byte
// budget is an error, never EOF; only two end blocks followed by bounded zero
// padding and underlying EOF prove the complete archive was received.
type buildArchiveReader struct {
	source     io.Reader
	limits     buildArchiveLimits
	consumed   int64
	metadata   int64
	headers    int
	block      [512]byte
	pending    []byte
	body       int64
	headerSize int64
	zeros      int
	ended      bool
}

func (reader *buildArchiveReader) readSource(data []byte) (int, error) {
	remaining := reader.limits.bytes - reader.consumed
	if remaining == 0 {
		var extra [1]byte
		count, err := reader.source.Read(extra[:])
		if count != 0 {
			return 0, errors.New("Docker archive raw byte limit exceeded")
		}
		return 0, err
	}
	if int64(len(data)) > remaining {
		data = data[:remaining]
	}
	count, err := reader.source.Read(data)
	reader.consumed += int64(count)
	return count, err
}
func (reader *buildArchiveReader) Read(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}
	if len(reader.pending) > 0 {
		count := copy(data, reader.pending)
		reader.pending = reader.pending[count:]
		return count, nil
	}
	if reader.body > 0 {
		if int64(len(data)) > reader.body {
			data = data[:reader.body]
		}
		count, err := reader.readSource(data)
		reader.body -= int64(count)
		return count, err
	}
	if reader.ended {
		return 0, io.EOF
	}
	if _, err := io.ReadFull(buildArchiveSource{reader}, reader.block[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return 0, errors.New("Docker archive missing end markers")
		}
		return 0, err
	}
	if err := reader.validateBlock(); err != nil {
		return 0, err
	}
	reader.pending = reader.block[:]
	return reader.Read(data)
}

type buildArchiveSource struct{ reader *buildArchiveReader }

func (source buildArchiveSource) Read(data []byte) (int, error) { return source.reader.readSource(data) }
func (reader *buildArchiveReader) validateBlock() error {
	zero := [512]byte{}
	if bytes.Equal(reader.block[:], zero[:]) {
		reader.zeros++
		reader.ended = reader.zeros == 2
		return nil
	}
	if reader.zeros != 0 {
		return errors.New("Docker archive has an incomplete end marker")
	}
	reader.headers++
	if reader.headers > reader.limits.headers {
		return errors.New("Docker archive header limit exceeded")
	}
	size, err := buildArchiveSize(reader.block[124:136])
	if err != nil {
		return err
	}
	if size > reader.limits.bytes {
		return errors.New("Docker archive raw byte limit exceeded")
	}
	reader.headerSize = size
	reader.body = (size + 511) / 512 * 512
	switch reader.block[156] {
	case tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
		if size > reader.limits.metadataBytes {
			return errors.New("Docker archive metadata body limit exceeded")
		}
		if size > reader.limits.metadataTotal-reader.metadata {
			return errors.New("Docker archive metadata total limit exceeded")
		}
		reader.metadata += size
	}
	return nil
}
func buildArchiveSize(field []byte) (int64, error) {
	if field[0]&0x80 != 0 {
		if field[0]&0x40 != 0 {
			return 0, errors.New("negative Docker archive size")
		}
		value := int64(field[0] & 0x7f)
		for _, part := range field[1:] {
			if value > (1<<63-1-int64(part))/256 {
				return 0, errors.New("Docker archive size overflow")
			}
			value = value*256 + int64(part)
		}
		return value, nil
	}
	value, err := strconv.ParseInt(strings.Trim(string(field), " \x00"), 8, 64)
	if err != nil || value < 0 {
		return 0, errors.New("invalid Docker archive size")
	}
	return value, nil
}
func (reader *buildArchiveReader) finish() error {
	if !reader.ended {
		return errors.New("Docker archive missing end markers")
	}
	buffer := make([]byte, 32*1024)
	for {
		count, err := reader.readSource(buffer)
		for _, value := range buffer[:count] {
			if value != 0 {
				return errors.New("Docker archive contains trailing data")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type buildArchiveLimits struct { bytes int64; headers int; metadataBytes, metadataTotal int64 }
