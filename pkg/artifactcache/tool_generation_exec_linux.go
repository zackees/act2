//go:build linux

package artifactcache

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
)

const toolGenerationLeaseFDEnvironment = "BOSN_TOOL_GENERATION_LEASE_FD"

func execWithToolGeneration(ctx context.Context, root, id string, maxBytes int64, command []string) error {
	if len(command) == 0 || len(command) > 64 {
		return fmt.Errorf("tool generation exec requires a bounded command")
	}
	total := 0
	for _, argument := range command {
		total += len(argument)
		if strings.ContainsRune(argument, '\x00') || total > 65536 {
			return fmt.Errorf("tool generation exec command is invalid or oversized")
		}
	}
	path, err := exec.LookPath(command[0])
	if err != nil {
		return err
	}
	var file *os.File
	lease, err := acquireToolGenerationLeaseWithReader(ctx, root, id, maxBytes, func(path string) (ToolGenerationLease, error) {
		return bbolt.Open(path, 0600, &bbolt.Options{ReadOnly: true, Timeout: 100 * time.Millisecond,
			OpenFile: func(name string, flags int, mode os.FileMode) (*os.File, error) {
				opened, openErr := openCoordinationFile(name, flags, mode)
				if openErr == nil {
					file = opened
				}
				return opened, openErr
			}})
	})
	if err != nil {
		return err
	}
	defer lease.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if file == nil {
		return fmt.Errorf("generation reader descriptor is unavailable")
	}
	fd := file.Fd()
	flags, err := unix.FcntlInt(fd, unix.F_GETFD, 0)
	if err != nil {
		return err
	}
	// Keep the exact original open-file description and its shared flock. No
	// close/reopen or unlock/relock window is permitted at the exec boundary.
	if _, err := unix.FcntlInt(fd, unix.F_SETFD, flags & ^unix.FD_CLOEXEC); err != nil {
		return err
	}
	defer func() { _, _ = unix.FcntlInt(fd, unix.F_SETFD, flags) }()
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, toolGenerationLeaseFDEnvironment+"=") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, toolGenerationLeaseFDEnvironment+"="+strconv.FormatUint(uint64(fd), 10))
	// #nosec G204 -- Caller explicitly supplies a bounded argv for the dedicated engine init; LookPath rejects unsafe relative PATH lookup. No shell is inserted.
	return syscall.Exec(path, command, environment)
}
