package container

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const ownedArchiveByteLimit int64 = 64 << 30

// GetOwnedTreeArchive streams private workspace state without buffering a tree
// in memory. Closing the reader releases the producer if Docker stops reading.
func (e *HostEnvironment) GetOwnedTreeArchive(ctx context.Context, source string) (io.ReadCloser, error) {
	if err := e.validateOwnedTree(source); err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			writer.CloseWithError(ctx.Err())
		case <-done:
		}
	}()
	go func() {
		defer close(done)
		bounded := &ownedArchiveWriter{ctx: ctx, writer: writer, remaining: ownedArchiveByteLimit}
		tarWriter := tar.NewWriter(bounded)
		collector := &ownedTarCollector{writer: tarWriter}
		err := walkOwnedArchive(ctx, source, collector)
		writer.CloseWithError(errors.Join(err, tarWriter.Close()))
	}()
	return reader, nil
}

func walkOwnedArchive(ctx context.Context, source string, collector *ownedTarCollector) error {
	root, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer root.Close()
	return fs.WalkDir(root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := root.Readlink(name)
			if err != nil {
				return err
			}
			return collector.WriteFile(name, info, link, nil)
		}
		if info.IsDir() {
			return collector.WriteFile(name, info, "", nil)
		}
		if !info.Mode().IsRegular() {
			return errors.New("unsupported entry in Docker action workspace")
		}
		file, err := root.Open(name)
		if err != nil {
			return err
		}
		return errors.Join(collector.WriteFile(name, info, "", file), file.Close())
	})
}

type ownedArchiveWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int64
}

func (w *ownedArchiveWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > w.remaining {
		return 0, errors.New("Docker action archive exceeds workspace limits")
	}
	w.remaining -= int64(len(data))
	return w.writer.Write(data)
}

type ownedTarCollector struct {
	writer   *tar.Writer
	entries  int
	expanded int64
}

func (c *ownedTarCollector) WriteFile(name string, info fs.FileInfo, link string, content io.Reader) error {
	c.entries++
	if c.entries > 1_000_000 {
		return errors.New("Docker action archive exceeds workspace entry limit")
	}
	if info.Mode().IsRegular() {
		if info.Size() < 0 || info.Size() > ownedArchiveByteLimit-c.expanded {
			return errors.New("Docker action archive exceeds workspace limits")
		}
		c.expanded += info.Size()
	}
	header, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	header.Name = filepath.ToSlash(name)
	header.Format = tar.FormatPAX // Retain nanosecond source timestamps.
	if err = c.writer.WriteHeader(header); err != nil {
		return err
	}
	if content != nil {
		_, err = io.Copy(c.writer, content)
	}
	return err
}
