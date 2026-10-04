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
	source           io.Reader
	limits           buildArchiveLimits
	consumed         int64
	metadata         int64
	headers          int
	block            [512]byte
	pending          []byte
	body             int64
	metadataType     byte
	paxName, gnuName string
	headerSize       int64
	zeros            int
	ended            bool
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
	if reader.body > 0 && reader.metadataType != 0 {
		payload := make([]byte, reader.body)
		if _, err := io.ReadFull(buildArchiveSource{reader}, payload); err != nil {
			return 0, err
		}
		if err := reader.inspectMetadata(payload[:reader.headerSize]); err != nil {
			return 0, err
		}
		reader.pending = payload
		reader.body = 0
		reader.metadataType = 0
		return reader.Read(data)
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

func (source buildArchiveSource) Read(data []byte) (int, error) {
	return source.reader.readSource(data)
}
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
	switch reader.block[156] {
	case tar.TypeReg, 0, tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
	case tar.TypeDir, tar.TypeSymlink, tar.TypeLink, tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		if size != 0 {
			return errors.New("bodyless Docker archive entry has nonzero size")
		}
	default:
		return errors.New("unsupported or sparse Docker build archive entry")
	}
	kind := reader.block[156]
	if err := reader.validateLegacyDirectory(kind, size); err != nil {
		return err
	}

	switch kind {
	case tar.TypeXHeader:
		reader.metadataType = kind
		reader.paxName = ""
	case tar.TypeXGlobalHeader:
		reader.metadataType = kind
		reader.paxName = ""
		reader.gnuName = ""
	case tar.TypeGNULongName:
		reader.metadataType = kind
		reader.gnuName = ""
	case tar.TypeGNULongLink:
		reader.metadataType = kind
	default:
		reader.metadataType = 0
		reader.paxName = ""
		reader.gnuName = ""
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

type buildArchiveLimits struct {
	bytes                        int64
	headers                      int
	metadataBytes, metadataTotal int64
}

// Inspect bounded PAX keys before archive/tar can allocate a sparse map.
type archivePAXMetadata struct{ name string }

func validatePAXBody(body []byte) (archivePAXMetadata, error) {
	var metadata archivePAXMetadata
	for len(body) > 0 {
		separator := bytes.IndexByte(body, ' ')
		if separator <= 0 {
			return archivePAXMetadata{}, errors.New("invalid PAX record length")
		}
		length, err := strconv.Atoi(string(body[:separator]))
		if err != nil || length <= separator+1 || length > len(body) || body[length-1] != '\n' {
			return archivePAXMetadata{}, errors.New("invalid PAX record framing")
		}
		key, value, ok := strings.Cut(string(body[separator+1:length-1]), "=")
		if !ok {
			return archivePAXMetadata{}, errors.New("invalid PAX record key")
		}
		if strings.HasPrefix(key, "GNU.sparse.") {
			return archivePAXMetadata{}, errors.New("unsupported sparse Docker archive metadata")
		}
		if key == "path" && value != "" {
			metadata.name = value
		}
		body = body[length:]
	}
	return metadata, nil
}

func (reader *buildArchiveReader) inspectMetadata(body []byte) error {
	switch reader.metadataType {
	case tar.TypeXHeader, tar.TypeXGlobalHeader:
		metadata, err := validatePAXBody(body)
		if err != nil {
			return err
		}
		if reader.metadataType == tar.TypeXHeader {
			reader.paxName = metadata.name
		}
	case tar.TypeGNULongName:
		name, _, _ := strings.Cut(string(body), "\x00")
		reader.gnuName = name
	}
	return nil
}

func (reader *buildArchiveReader) validateLegacyDirectory(kind byte, size int64) error {
	if kind != 0 || size == 0 {
		return nil
	}
	{
		name, err := legacyArchiveHeaderName(reader.block)
		if err != nil {
			return err
		}
		if reader.paxName != "" {
			name = reader.paxName
		}
		if reader.gnuName != "" {
			name = reader.gnuName
		}
		if strings.HasSuffix(name, "/") {
			return errors.New("bodyless legacy archive directory has nonzero size")
		}
	}
	return nil
}

// Resolve format-specific names with the same parser as the real decoder.
// A private fixed-size regular, zero-body header cannot trigger PAX, GNU
// metadata or sparse allocation; GNU timestamp fields retain their meaning.
func legacyArchiveHeaderName(block [512]byte) (string, error) {
	block[156] = tar.TypeReg
	copy(block[124:136], []byte("00000000000\x00"))
	for i := 148; i < 156; i++ {
		block[i] = ' '
	}
	checksum := 0
	for _, value := range block {
		checksum += int(value)
	}
	field := strconv.FormatInt(int64(checksum), 8)
	copy(block[148:156], []byte(strings.Repeat("0", 6-len(field))+field+"\x00 "))
	header, err := tar.NewReader(bytes.NewReader(block[:])).Next()
	if err != nil {
		return "", err
	}
	return header.Name, nil
}
