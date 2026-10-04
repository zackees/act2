//go:build !(WITHOUT_DOCKER || !(linux || darwin || windows || netbsd))

package container

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestBuildArchiveRejectsHiddenMetadataBodylessHeader(t *testing.T) {
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
			guard := &buildArchiveReader{source: bytes.NewReader(data), limits: buildArchiveLimits{bytes: 4096, headers: 1, metadataBytes: 16, metadataTotal: 16}}
			_, err := tar.NewReader(guard).Next()
			require.Error(t, err, "reject before bodyless size can hide metadata/header accounting")
		})
	}
}

type stalledBuildContext struct {
	started, closed      chan struct{}
	startOnce, closeOnce sync.Once
}

func (source *stalledBuildContext) Read([]byte) (int, error) {
	source.startOnce.Do(func() { close(source.started) })
	<-source.closed
	return 0, io.EOF
}
func (source *stalledBuildContext) Close() error {
	source.closeOnce.Do(func() { close(source.closed) })
	return nil
}
func TestBuildContextCancellationInterruptsReaderAndCleans(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("API-Version", "1.48")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	t.Setenv("DOCKER_HOST", server.URL)
	t.Setenv("DOCKER_API_VERSION", "")
	source := &stalledBuildContext{started: make(chan struct{}), closed: make(chan struct{})}
	defer source.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- NewDockerBuildExecutor(NewDockerBuildExecutorInput{BuildContext: source, Dockerfile: "Dockerfile", ImageTag: "cancelled", Platform: "linux/amd64"})(ctx)
	}()
	select {
	case <-source.started:
	case <-time.After(time.Second):
		t.Fatal("context reader did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(200 * time.Millisecond):
		_ = source.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("manual release did not unblock baseline")
		}
		t.Error("cancel did not interrupt the context reader")
	}
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
	select {
	case <-source.closed:
	default:
		t.Error("cancel must close interruptible source")
	}
}
func TestBuildArchiveRejectsUnsupportedSparseType(t *testing.T) {
	var body bytes.Buffer
	writer := tar.NewWriter(&body)
	require.NoError(t, writer.WriteHeader(&tar.Header{Name: "file", Mode: 0644, Size: 0}))
	require.NoError(t, writer.Close())
	data := body.Bytes()
	data[156] = tar.TypeGNUSparse
	guard := &buildArchiveReader{source: strings.NewReader(string(data)), limits: buildArchiveLimits{bytes: 4096, headers: 10, metadataBytes: 16, metadataTotal: 16}}
	_, err := guard.Read(make([]byte, 512))
	require.Error(t, err)
}

func TestBuildArchivePAXSparseRejectedBeforeDecoder(t *testing.T) {
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
	guard := &buildArchiveReader{source: bytes.NewReader(data), limits: buildArchiveLimits{bytes: 4096, headers: 10, metadataBytes: 128, metadataTotal: 128}}
	header := make([]byte, 512)
	_, err = io.ReadFull(guard, header)
	require.NoError(t, err)
	_, err = guard.Read(make([]byte, 512))
	require.Error(t, err, "sparse metadata must be rejected before decoder allocation")
}

func TestBuildArchiveLegacyDirectoryFraming(t *testing.T) {
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
				data.Write(legacyBuildTestEntry(t, "metadata", kind, payload, int64(len(payload))))
			}
			data.Write(legacyBuildTestEntry(t, name, 0, nil, 1024))
			data.Write(make([]byte, 1024))
			guard := &buildArchiveReader{source: bytes.NewReader(data.Bytes()), limits: buildArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
			_, err := tar.NewReader(guard).Next()
			require.Error(t, err, "legacy directory must not hide the next raw headers as a file body")
		})
	}
}
func legacyBuildTestEntry(t *testing.T, name string, kind byte, payload []byte, size int64) []byte {
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

func TestBuildArchiveLegacyRegularAndEmptyDirectory(t *testing.T) {
	for _, test := range []struct {
		name string
		body []byte
	}{{"ordinary", []byte("finite")}, {"empty/", nil}} {
		data := legacyBuildTestEntry(t, test.name, 0, test.body, int64(len(test.body)))
		data = append(data, make([]byte, 1024)...)
		guard := &buildArchiveReader{source: bytes.NewReader(data), limits: buildArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
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

func TestBuildArchiveEmptyMetadataReplacesPriorName(t *testing.T) {
	for _, kind := range []byte{tar.TypeXHeader, tar.TypeGNULongName} {
		t.Run(fmt.Sprintf("type-%c", kind), func(t *testing.T) {
			payload := []byte("19 path=directory/\n")
			if kind == tar.TypeGNULongName {
				payload = []byte("directory/\x00")
			}
			data := legacyBuildTestEntry(t, "metadata", kind, payload, int64(len(payload)))
			data = append(data, legacyBuildTestEntry(t, "metadata", kind, nil, 0)...)
			data = append(data, legacyBuildTestEntry(t, "ordinary", 0, []byte("finite"), 6)...)
			data = append(data, make([]byte, 1024)...)
			guard := &buildArchiveReader{source: bytes.NewReader(data), limits: buildArchiveLimits{bytes: 8192, headers: 16, metadataBytes: 128, metadataTotal: 128}}
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
