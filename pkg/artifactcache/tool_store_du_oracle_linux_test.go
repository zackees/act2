//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// GNU du 9.2 stopped counting directory st_size in apparent bytes. The tool
// store's contract includes that metadata, so supplement newer du output using
// independent GNU find, never the auditor's own traversal or calculations.
// https://lists.gnu.org/archive/html/coreutils-announce/2023-03/msg00000.html
func independentDuDirectorySupplement(ctx context.Context, root string) (int64, error) {
	version, err := exec.CommandContext(ctx, "du", "--version").Output()
	if err != nil {
		return 0, err
	}
	excludesDirectories, err := gnuDuExcludesDirectorySizes(string(version))
	if err != nil || !excludesDirectories {
		return 0, err
	}
	// #nosec G204 -- Fixed GNU find flags over this test's private tree, without a shell or symlink following.
	metadata, err := exec.CommandContext(ctx, "find", root, "-type", "d", "-printf", "%s\n").Output()
	if err != nil {
		return 0, err
	}
	return sumIndependentStatSizes(string(metadata))
}

func gnuDuExcludesDirectorySizes(version string) (bool, error) {
	line, _, _ := strings.Cut(version, "\n")
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "du" || fields[1] != "(GNU" || fields[2] != "coreutils)" {
		return false, fmt.Errorf("independent apparent-byte oracle requires GNU du")
	}
	parts := strings.Split(fields[3], ".")
	if len(parts) < 2 {
		return false, fmt.Errorf("GNU du version lacks major/minor components")
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 0 {
		return false, fmt.Errorf("invalid GNU du major version")
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil || minor < 0 {
		return false, fmt.Errorf("invalid GNU du minor version")
	}
	return major > 9 || major == 9 && minor >= 2, nil
}

func sumIndependentStatSizes(output string) (int64, error) {
	var total int64
	for _, field := range strings.Fields(output) {
		size, err := strconv.ParseInt(field, 10, 64)
		if err != nil || size < 0 || size > math.MaxInt64-total {
			return 0, fmt.Errorf("independent metadata oracle produced invalid size")
		}
		total += size
	}
	return total, nil
}

func TestToolStoreDuDirectorySemanticsVersionBoundary(t *testing.T) {
	for _, sample := range []struct {
		version string
		want    bool
	}{
		{"8.32", false}, {"9.1", false}, {"9.2", true}, {"9.4", true}, {"10.0", true},
	} {
		t.Run(sample.version, func(t *testing.T) {
			actual, err := gnuDuExcludesDirectorySizes("du (GNU coreutils) " + sample.version + "\n")
			require.NoError(t, err)
			require.Equal(t, sample.want, actual)
		})
	}
	for _, invalid := range []string{"du (BusyBox) 9.4", "du (GNU coreutils) unknown", "du (GNU coreutils) 9.x"} {
		_, err := gnuDuExcludesDirectorySizes(invalid)
		require.Error(t, err, "unknown oracle behavior must never silently pass")
	}
}

func TestToolStoreDuOracleNormalizesDirectoryMetadata(t *testing.T) {
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	require.NoError(t, os.Mkdir(empty, 0755))
	payload := filepath.Join(root, "payload")
	require.NoError(t, os.WriteFile(payload, []byte("abc"), 0600))
	require.NoError(t, os.Link(payload, filepath.Join(root, "hardlink")))
	require.NoError(t, os.Symlink("payload", filepath.Join(root, "link")))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// #nosec G204 -- Fixed GNU stat flags over two private fixture directories.
	metadata, err := exec.CommandContext(ctx, "stat", "--format=%s", root, empty).Output()
	require.NoError(t, err)
	directoryBytes, err := sumIndependentStatSizes(string(metadata))
	require.NoError(t, err)
	// #nosec G204 -- Fixed independent GNU du oracle over the private fixture.
	output, err := exec.CommandContext(ctx, "du", "--summarize", "--apparent-size", "--block-size=1", root).Output()
	require.NoError(t, err)
	apparent, err := strconv.ParseInt(strings.Fields(string(output))[0], 10, 64)
	require.NoError(t, err)
	supplement, err := independentDuDirectorySupplement(ctx, root)
	require.NoError(t, err)
	// The three-byte payload is hardlinked and counts once; symlink text is
	// seven bytes. No auditor or internal traversal participates in this proof.
	require.Equal(t, directoryBytes+3+7, apparent+supplement)
}
