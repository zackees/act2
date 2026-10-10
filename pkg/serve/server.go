package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

// Config is the server's fixed configuration.
type Config struct {
	Socket      string
	RunLabel    string
	ScopePrefix string
	WorkRoot    string
	PortBase    int
	MaxRuns     int
	ActBinary   string
	Version     string
}

// Validate refuses a configuration whose ports or names cannot work.
func (c Config) Validate() error {
	switch {
	case c.RunLabel == "":
		return errors.New("--run-label is required")
	case c.ScopePrefix == "":
		return errors.New("--scope-prefix is required")
	case c.MaxRuns < 1:
		return errors.New("--max-runs must be at least 1")
	case c.PortBase < 1024 || c.PortBase+2*c.MaxRuns > 65535:
		return errors.New("--port-base and --max-runs must keep every port in 1024..65535")
	case !filepath.IsAbs(c.WorkRoot):
		return errors.New("--work-root must be absolute")
	}
	return nil
}

// workDirs is every run's work tree layout.
var workDirs = []string{"src", "overlay", "artifacts", "home/.cache", "home/.config", "tmp"}

type run struct {
	scope   Scope
	proc    *os.Process
	killed  bool          // a kill arrived for the current exec
	execing chan struct{} // closed when the current exec ends; nil when idle
}

// Server admits runs and executes act inside their scopes.
type Server struct {
	cfg      Config
	isolator Isolator
	mu       sync.Mutex
	runs     map[string]*run
	slots    []string
}

// NewServer validates cfg and returns an idle server.
func NewServer(cfg Config, isolator Isolator) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Server{cfg: cfg, isolator: isolator, runs: map[string]*run{}, slots: make([]string, cfg.MaxRuns)}, nil
}

// Handler is the HTTP API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.health)
	mux.HandleFunc("POST /v1/runs", s.admit)
	mux.HandleFunc("POST /v1/runs/{id}/exec", s.exec)
	mux.HandleFunc("POST /v1/runs/{id}/cancel", s.cancel)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.close)
	return mux
}

// Serve reaps what a previous server left, then serves on the socket until
// ctx ends.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.isolator.Reap(ctx); err != nil {
		return fmt.Errorf("reap previous runs: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfg.Socket), 0o700); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.Socket)
	listener, err := net.Listen("unix", s.cfg.Socket)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.cfg.Socket, 0o600); err != nil {
		listener.Close()
		return err
	}
	server := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		_ = server.Close()
	}()
	log.Infof("act serve listening on %s", s.cfg.Socket)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func reply(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func fail(w http.ResponseWriter, status int, err error) {
	reply(w, status, ErrorBody{Error: err.Error()})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	runs := make([]string, 0, len(s.runs))
	for id := range s.runs {
		runs = append(runs, id)
	}
	s.mu.Unlock()
	reply(w, http.StatusOK, Health{Protocol: ProtocolVersion, Version: s.cfg.Version, MaxRuns: s.cfg.MaxRuns, Runs: runs})
}

func (s *Server) scopeFor(runID string, slot int, limits Limits) (Scope, error) {
	key, err := RunKey(runID)
	if err != nil {
		return Scope{}, err
	}
	name := s.cfg.ScopePrefix + key
	return Scope{
		RunID:        runID,
		Key:          key,
		Slot:         slot,
		Cgroup:       "/" + name,
		Network:      name,
		Work:         filepath.Join(s.cfg.WorkRoot, key),
		ArtifactPort: s.cfg.PortBase + 2*slot,
		CachePort:    s.cfg.PortBase + 2*slot + 1,
		Limits:       limits,
	}, nil
}

// reserve admits runID into a free slot, or returns its existing scope.
func (s *Server) reserve(req AdmitRequest) (Scope, bool, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.runs[req.RunID]; ok {
		if existing.scope.Limits != req.Limits {
			return Scope{}, false, http.StatusConflict, errors.New("run is already admitted with other limits")
		}
		return existing.scope, true, http.StatusOK, nil
	}
	for slot, holder := range s.slots {
		if req.Slot != nil && *req.Slot != slot {
			continue
		}
		if holder != "" {
			if req.Slot != nil {
				return Scope{}, false, http.StatusConflict, fmt.Errorf("slot %d is taken", slot)
			}
			continue
		}
		scope, err := s.scopeFor(req.RunID, slot, req.Limits)
		if err != nil {
			return Scope{}, false, http.StatusBadRequest, err
		}
		for _, other := range s.runs {
			if other.scope.Key == scope.Key {
				return Scope{}, false, http.StatusConflict, errors.New("another run has the same key")
			}
		}
		s.slots[slot] = req.RunID
		s.runs[req.RunID] = &run{scope: scope}
		return scope, false, http.StatusCreated, nil
	}
	if req.Slot != nil {
		return Scope{}, false, http.StatusConflict, fmt.Errorf("slot %d is out of range", *req.Slot)
	}
	return Scope{}, false, http.StatusServiceUnavailable, fmt.Errorf("all %d run slots are taken", len(s.slots))
}

func (s *Server) release(runID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.runs[runID]; ok {
		s.slots[r.scope.Slot] = ""
		delete(s.runs, runID)
	}
}

func (s *Server) admit(w http.ResponseWriter, r *http.Request) {
	var req AdmitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if _, err := RunKey(req.RunID); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := req.Limits.Validate(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	scope, existed, status, err := s.reserve(req)
	if err != nil {
		fail(w, status, err)
		return
	}
	if !existed {
		if err := s.open(r.Context(), scope); err != nil {
			// Whatever Open made is removed before the slot is freed.
			if cerr := s.isolator.Close(context.WithoutCancel(r.Context()), scope); cerr == nil {
				s.release(scope.RunID)
			}
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	reply(w, status, scope)
}

func (s *Server) open(ctx context.Context, scope Scope) error {
	for _, dir := range workDirs {
		if err := os.MkdirAll(filepath.Join(scope.Work, dir), 0o755); err != nil {
			return err
		}
	}
	return s.isolator.Open(ctx, scope)
}

func (s *Server) lookup(id string) (*run, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.runs[id]
	return r, ok
}

// baseEnv is what every act process inherits besides its request's env.
func baseEnv(work string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(work, "home"),
		"XDG_CACHE_HOME=" + filepath.Join(work, "home/.cache"),
		"XDG_CONFIG_HOME=" + filepath.Join(work, "home/.config"),
		"TMPDIR=" + filepath.Join(work, "tmp"),
	}
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request) {
	var req ExecRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	run, ok := s.runs[id]
	if ok && run.execing != nil {
		s.mu.Unlock()
		fail(w, http.StatusConflict, errors.New("the run is already executing"))
		return
	}
	if !ok {
		s.mu.Unlock()
		fail(w, http.StatusNotFound, errors.New("no such run"))
		return
	}
	done := make(chan struct{})
	run.execing = done
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		run.execing, run.proc, run.killed = nil, nil, false
		s.mu.Unlock()
		close(done)
	}()

	cmd := exec.Command(s.cfg.ActBinary, req.Args...) //nolint:gosec // the server runs its own act binary for a trusted local caller on a 0600 socket
	cmd.Dir = filepath.Join(run.scope.Work, "src")
	cmd.Env = baseEnv(run.scope.Work)
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	stream := newFrameWriter(w)
	cmd.Stdout = stream.writer("stdout")
	cmd.Stderr = stream.writer("stderr")
	started, err := s.isolator.Prepare(cmd, run.scope)
	if err == nil {
		err = cmd.Start()
		started()
	}
	if err != nil {
		stream.exit(Exit{Code: -1, Error: err.Error()})
		return
	}
	s.mu.Lock()
	run.proc = cmd.Process
	killedEarly := run.killed
	s.mu.Unlock()
	if killedEarly {
		s.kill(run)
	}
	var timedOut atomic.Bool
	if req.DeadlineSecs > 0 {
		timer := time.AfterFunc(time.Duration(req.DeadlineSecs)*time.Second, func() {
			timedOut.Store(true)
			s.kill(run)
		})
		defer timer.Stop()
	}
	waitErr := cmd.Wait()
	stream.exit(exitOf(cmd, waitErr, timedOut.Load()))
}

func exitOf(cmd *exec.Cmd, waitErr error, timedOut bool) Exit {
	end := Exit{TimedOut: timedOut}
	if cmd.ProcessState == nil {
		end.Code, end.Error = -1, fmt.Sprint(waitErr)
		return end
	}
	end.Code = cmd.ProcessState.ExitCode()
	if status, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		end.Signal = status.Signal().String()
		end.Code = 128 + int(status.Signal())
	}
	return end
}

// kill stops the run's processes: its whole cgroup, and its act process.
func (s *Server) kill(run *run) {
	if err := s.isolator.Kill(run.scope); err != nil {
		log.Warnf("kill run %s: %v", run.scope.RunID, err)
	}
	s.mu.Lock()
	proc := run.proc
	run.killed = run.execing != nil
	s.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}
}

func (s *Server) cancel(w http.ResponseWriter, r *http.Request) {
	run, ok := s.lookup(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("no such run"))
		return
	}
	s.kill(run)
	reply(w, http.StatusOK, run.scope)
}

func (s *Server) close(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, ok := s.lookup(id)
	if !ok {
		// Closing is idempotent: a closed or never-admitted run is gone.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.kill(run)
	s.mu.Lock()
	done := run.execing
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			fail(w, http.StatusInternalServerError, errors.New("the run's act process did not stop"))
			return
		}
	}
	if err := s.isolator.Close(r.Context(), run.scope); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.release(id)
	w.WriteHeader(http.StatusNoContent)
}
