package artifactcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type importReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r importReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func copyVerifiedArchive(ctx context.Context, source, destination string, size int64) (string, error) {
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return "", err
	}
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	copied, copyErr := io.Copy(io.MultiWriter(output, hash), importReader{ctx: ctx, reader: io.LimitReader(input, size+1)})
	syncErr := output.Sync()
	closeErr := output.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if copied != size {
		return "", fmt.Errorf("archive changed length during import")
	}
	if syncErr != nil {
		return "", syncErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	// Re-read source to detect legacy writes outside metadata coordination, and
	// verify the actual destination bytes before publishing its namespace.
	for _, path := range []string{source, destination} {
		checksum, err := archiveChecksum(ctx, path, size)
		if err != nil {
			return "", err
		}
		if checksum != digest {
			return "", fmt.Errorf("archive changed during checksum verification")
		}
	}
	return digest, nil
}

func archiveChecksum(ctx context.Context, path string, size int64) (string, error) {
	// #nosec G703 -- Callers validate canonical source/destination paths before verified archive transfer.
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, importReader{ctx: ctx, reader: io.LimitReader(file, size+1)})
	if err != nil {
		return "", err
	}
	if n != size {
		return "", fmt.Errorf("archive length changed during verification")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
