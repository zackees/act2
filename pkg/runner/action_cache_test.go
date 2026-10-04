package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

//nolint:gosec
func TestActionCache(t *testing.T) {
	before := archiveProducerGoroutines()
	a := assert.New(t)
	cache := &GoGitActionCache{
		Path: os.TempDir(),
	}
	ctx := context.Background()
	cacheDir := "nektos/act-test-actions"
	repo := "https://github.com/nektos/act-test-actions"
	refs := []struct {
		Name     string
		CacheDir string
		Repo     string
		Ref      string
	}{
		{
			Name:     "Fetch Branch Name",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "main",
		},
		{
			Name:     "Fetch Branch Name Absolutely",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "refs/heads/main",
		},
		{
			Name:     "Fetch HEAD",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "HEAD",
		},
		{
			Name:     "Fetch Sha",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "de984ca37e4df4cb9fd9256435a3b82c4a2662b1",
		},
	}
	for _, c := range refs {
		t.Run(c.Name, func(_ *testing.T) {
			sha, err := cache.Fetch(ctx, c.CacheDir, c.Repo, c.Ref, "")
			if !a.NoError(err) || !a.NotEmpty(sha) {
				return
			}
			atar, err := cache.GetTarArchive(ctx, c.CacheDir, sha, "js")
			if !a.NoError(err) || !a.NotEmpty(atar) {
				return
			}
			defer atar.Close()
			mytar := tar.NewReader(atar)
			th, err := mytar.Next()
			if !a.NoError(err) || !a.NotEqual(0, th.Size) {
				return
			}
			buf := &bytes.Buffer{}
			// G110: Potential DoS vulnerability via decompression bomb (gosec)
			_, err = io.Copy(buf, mytar)
			a.NoError(err)
			str := buf.String()
			a.NotEmpty(str)
		})
	}
	a.Eventually(func() bool {
		return archiveProducerGoroutines() == before
	}, time.Second, time.Millisecond, "archive producers and cancellation waiters must terminate after each case")
}

func archiveProducerGoroutines() int {
	stack := make([]byte, 512<<10)
	n := runtime.Stack(stack, true)
	return strings.Count(string(stack[:n]), "GoGitActionCache.GetTarArchive.func")
}

func TestActionCacheFailures(t *testing.T) {
	a := assert.New(t)
	cache := &GoGitActionCache{
		Path: os.TempDir(),
	}
	ctx := context.Background()
	cacheDir := "nektos/act-test-actions"
	repo := "https://github.com/nektos/act-test-actions-not-exist"
	repoExist := "https://github.com/nektos/act-test-actions"
	refs := []struct {
		Name     string
		CacheDir string
		Repo     string
		Ref      string
	}{
		{
			Name:     "Fetch Branch Name",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "main",
		},
		{
			Name:     "Fetch Branch Name Absolutely",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "refs/heads/main",
		},
		{
			Name:     "Fetch HEAD",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "HEAD",
		},
		{
			Name:     "Fetch Sha",
			CacheDir: cacheDir,
			Repo:     repo,
			Ref:      "de984ca37e4df4cb9fd9256435a3b82c4a2662b1",
		},
		{
			Name:     "Fetch Branch Name no existing",
			CacheDir: cacheDir,
			Repo:     repoExist,
			Ref:      "main2",
		},
		{
			Name:     "Fetch Branch Name Absolutely no existing",
			CacheDir: cacheDir,
			Repo:     repoExist,
			Ref:      "refs/heads/main2",
		},
		{
			Name:     "Fetch Sha no existing",
			CacheDir: cacheDir,
			Repo:     repoExist,
			Ref:      "de984ca37e4df4cb9fd9256435a3b82c4a2662b2",
		},
	}
	for _, c := range refs {
		t.Run(c.Name, func(t *testing.T) {
			_, err := cache.Fetch(ctx, c.CacheDir, c.Repo, c.Ref, "")
			t.Logf("%s\n", err)
			if !a.Error(err) {
				return
			}
		})
	}
}
