package runner

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostArchiveRejectsHiddenMetadataBodylessHeader(t *testing.T) {
	for _, kind := range []byte{tar.TypeDir, tar.TypeSymlink, tar.TypeLink} {
		t.Run(fmt.Sprintf("type-%c", kind), func(t *testing.T) {
			// The decoder ignores bodyless Size; raw framing must not skip the
			// following PAX headers as though they were opaque body bytes.
			var body bytes.Buffer
			writer := tar.NewWriter(&body)
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: "bodyless", Mode: 0755, Typeflag: kind, Linkname: "target"}))
			require.NoError(t, writer.Close())
			data := body.Bytes()
			copy(data[124:136], []byte("00000002000\x00"))
			for i := 148; i < 156; i++ {
				data[i] = ' '
			}
			checksum := 0
			for _, value := range data[:512] {
				checksum += int(value)
			}
			copy(data[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
			guard := &hostArchiveReader{source: bytes.NewReader(data), limits: hostArchiveLimits{bytes: 4096, headers: 1, metadataBytes: 16, metadataTotal: 16}}
			_, err := tar.NewReader(guard).Next()
			require.Error(t, err, "reject before bodyless size can hide metadata/header accounting")
		})
	}
}

func TestHostArchivePAXSparseRejectedBeforeDecoder(t *testing.T) {
	record := []byte("21 GNU.sparse.major=1\n")
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "metadata", Mode: 0600, Size: int64(len(record))}))
	_, err := writer.Write(record)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	data := buffer.Bytes()
	data[156] = tar.TypeXHeader
	for i := 148; i < 156; i++ {
		data[i] = ' '
	}
	checksum := 0
	for _, value := range data[:512] {
		checksum += int(value)
	}
	copy(data[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	guard := &hostArchiveReader{source: bytes.NewReader(data), limits: hostArchiveLimits{bytes: 4096, headers: 10, metadataBytes: 128, metadataTotal: 128}}
	header := make([]byte, 512)
	_, err = io.ReadFull(guard, header)
	require.NoError(t, err)
	_, err = guard.Read(make([]byte, 512))
	require.Error(t, err, "sparse metadata must be rejected before decoder allocation")
}

func TestHostArchiveLegacyDirectoryFraming(t *testing.T) {
	for _, override := range []string{"raw", "pax", "gnu"} {
		t.Run(override, func(t *testing.T) {
			var data bytes.Buffer
			name := "directory/"
			if override != "raw" {
				name = "regular"
				payload := []byte("19 path=directory/\n")
				kind := byte(tar.TypeXHeader)
				if override == "gnu" {
					payload = []byte("directory/\x00")
					kind = tar.TypeGNULongName
				}
				data.Write(legacyHostTestEntry(t, "metadata", kind, payload, int64(len(payload))))
			}
			data.Write(legacyHostTestEntry(t, name, 0, nil, 1024))
			data.Write(make([]byte, 1024))
			guard := &hostArchiveReader{source: bytes.NewReader(data.Bytes()), limits: hostArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
			_, err := tar.NewReader(guard).Next()
			require.Error(t, err, "legacy directory must not hide the next raw headers as a file body")
		})
	}
}
func legacyHostTestEntry(t *testing.T, name string, kind byte, payload []byte, size int64) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := tar.NewWriter(&buffer)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(payload))}))
	_, err := writer.Write(payload)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	data := buffer.Bytes()[:512+(len(payload)+511)/512*512]
	data[156] = kind
	copy(data[124:136], []byte(fmt.Sprintf("%011o\x00", size)))
	for i := 148; i < 156; i++ {
		data[i] = ' '
	}
	checksum := 0
	for _, value := range data[:512] {
		checksum += int(value)
	}
	copy(data[148:156], []byte(fmt.Sprintf("%06o\x00 ", checksum)))
	return data
}

func TestHostArchiveLegacyRegularAndEmptyDirectory(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{{"ordinary", []byte("finite")}, {"empty/", nil}} {
		data := legacyHostTestEntry(t, test.name, 0, test.body, int64(len(test.body)))
		data = append(data, make([]byte, 1024)...)
		guard := &hostArchiveReader{source: bytes.NewReader(data), limits: hostArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
		decoder := tar.NewReader(guard)
		header, err := decoder.Next()
		require.NoError(t, err)
		require.Equal(t, test.name, header.Name)
		body, err := io.ReadAll(decoder)
		require.NoError(t, err)
		require.Equal(t, string(test.body), string(body))
		_, err = decoder.Next()
		require.ErrorIs(t, err, io.EOF)
		require.NoError(t, guard.finish())
	}
	metadata, err := validatePAXBody([]byte("28 comment=GNU.sparse.major\n"))
	require.NoError(t, err, "sparse text in a value is ordinary metadata")
	require.Empty(t, metadata.name)
}

func TestHostArchiveEmptyMetadataReplacesPriorName(t *testing.T) {
	for _, kind := range []byte{tar.TypeXHeader, tar.TypeGNULongName} {
		t.Run(fmt.Sprintf("type-%c", kind), func(t *testing.T) {
			payload := []byte("19 path=directory/\n")
			if kind == tar.TypeGNULongName {
				payload = []byte("directory/\x00")
			}
			data := legacyHostTestEntry(t, "metadata", kind, payload, int64(len(payload)))
			data = append(data, legacyHostTestEntry(t, "metadata", kind, nil, 0)...)
			data = append(data, legacyHostTestEntry(t, "ordinary", 0, []byte("finite"), 6)...)
			data = append(data, make([]byte, 1024)...)
			guard := &hostArchiveReader{source: bytes.NewReader(data), limits: hostArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
			decoder := tar.NewReader(guard)
			header, err := decoder.Next()
			require.NoError(t, err)
			require.Equal(t, "ordinary", header.Name)
			body, err := io.ReadAll(decoder)
			require.NoError(t, err)
			require.Equal(t, "finite", string(body))
		})
	}
}
