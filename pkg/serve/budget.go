package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"time"

	log "github.com/sirupsen/logrus"
)

// The shared cache budget the server applies while it runs: every interval
// it runs act's own per-namespace retention (`act cache prune --apply`, the
// code the cache server runs in-process) over each coordinated namespace in
// the cache root, under the root's maintenance lock.
//
//   - Liveness: act takes each namespace's transfer lock and bolt.db with a
//     short timeout; a live cache server or transfer makes it report busy and
//     nothing in it is touched. Archives used within five minutes are kept.
//   - Only direct 16-hex children holding transfers.bolt are visited; a
//     namespace without act's transfer coordination is skipped, never deleted.
//   - The lock excludes any other maintenance of the same root; when it is
//     held the pass is skipped and reported busy.

// Budget is the per-namespace retention policy.
type Budget struct {
	Root          string
	Lock          string
	MaxBytes      int64
	UnusedAge     time.Duration
	MaxAge        time.Duration
	Interval      time.Duration
	MaxNamespaces int
}

// BudgetPass is the last pass's outcome.
type BudgetPass struct {
	At         time.Time         `json:"at"`
	Status     string            `json:"status"` // ok, busy, partial, failed
	Namespaces int               `json:"namespaces"`
	Skipped    []string          `json:"skipped,omitempty"`
	Error      string            `json:"error,omitempty"`
	Audits     []json.RawMessage `json:"audits,omitempty"`
}

var namespaceName = regexp.MustCompile(`^[0-9a-f]{16}$`)

func (b Budget) pruneArgs(dir string) []string {
	return []string{"cache", "prune", "--apply", "--cache-server-path", dir,
		"--cache-server-max-bytes", fmt.Sprint(b.MaxBytes),
		"--cache-server-max-age", b.MaxAge.String(),
		"--cache-server-unused-age", b.UnusedAge.String(),
		"--cache-server-gc-interval", b.Interval.String()}
}

// runBudgetPass applies the budget once.
func runBudgetPass(ctx context.Context, act string, b Budget) BudgetPass {
	pass := BudgetPass{At: time.Now().UTC(), Status: "ok"}
	if info, err := os.Lstat(b.Root); err != nil || !info.IsDir() {
		return pass
	}
	lock, err := os.OpenFile(filepath.Join(b.Root, b.Lock), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		pass.Status, pass.Error = "failed", err.Error()
		return pass
	}
	defer lock.Close()
	unlock, err := tryLock(lock)
	if err != nil {
		pass.Status = "busy"
		return pass
	}
	defer unlock()
	entries, err := os.ReadDir(b.Root)
	if err != nil {
		pass.Status, pass.Error = "failed", err.Error()
		return pass
	}
	for _, entry := range entries {
		dir := filepath.Join(b.Root, entry.Name())
		if !namespaceName.MatchString(entry.Name()) || !entry.IsDir() {
			continue
		}
		if info, err := os.Lstat(filepath.Join(dir, "transfers.bolt")); err != nil || !info.Mode().IsRegular() {
			pass.Skipped = append(pass.Skipped, entry.Name())
			continue
		}
		if pass.Namespaces >= b.MaxNamespaces {
			pass.Status = "partial"
			break
		}
		pass.Namespaces++
		var stdout, stderr bytes.Buffer
		cmd := exec.CommandContext(ctx, act, b.pruneArgs(dir)...) //nolint:gosec // the server's own act binary on a validated namespace path
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			pass.Status = "failed"
			pass.Error = fmt.Sprintf("%s: %v: %s", entry.Name(), err, bytes.TrimSpace(stderr.Bytes()))
		}
		scanner := bufio.NewScanner(&stdout)
		for scanner.Scan() {
			if line := bytes.TrimSpace(scanner.Bytes()); json.Valid(line) {
				pass.Audits = append(pass.Audits, append(json.RawMessage(nil), line...))
			}
		}
	}
	return pass
}

// runBudget applies the budget now and every interval until ctx ends.
func (s *Server) runBudget(ctx context.Context) {
	if s.cfg.Budget.Root == "" {
		return
	}
	for {
		pass := runBudgetPass(ctx, s.cfg.ActBinary, s.cfg.Budget)
		if pass.Status != "ok" {
			log.Warnf("cache budget pass: %s %s", pass.Status, pass.Error)
		}
		s.mu.Lock()
		s.budget = &pass
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.cfg.Budget.Interval):
		}
	}
}
