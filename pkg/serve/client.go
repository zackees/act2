package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
)

// Client talks to a server over its Unix socket.
type Client struct {
	http *http.Client
}

// NewClient returns a client for the server listening on socket.
func NewClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{http: &http.Client{Transport: transport}}
}

func (c *Client) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://act2"+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		var failure ErrorBody
		_ = json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&failure)
		return nil, &StatusError{Status: resp.StatusCode, Message: failure.Error}
	}
	return resp, nil
}

// StatusError is a non-2xx server reply.
type StatusError struct {
	Status  int
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("act serve: %d %s: %s", e.Status, http.StatusText(e.Status), e.Message)
}

func (c *Client) decode(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Health reports the server's state.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.decode(ctx, http.MethodGet, "/v1/health", nil, &h)
}

// Admit admits a run and returns its scope.
func (c *Client) Admit(ctx context.Context, req AdmitRequest) (Scope, error) {
	var scope Scope
	return scope, c.decode(ctx, http.MethodPost, "/v1/runs", req, &scope)
}

// Cancel kills a run's processes.
func (c *Client) Cancel(ctx context.Context, runID string) error {
	return c.decode(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/cancel", nil, nil)
}

// Close removes a run and everything it left.
func (c *Client) Close(ctx context.Context, runID string) error {
	return c.decode(ctx, http.MethodDelete, "/v1/runs/"+url.PathEscape(runID), nil, nil)
}

// Exec runs act in a run's scope, copying its output to stdout and stderr,
// and returns how it ended.
func (c *Client) Exec(ctx context.Context, runID string, req ExecRequest, stdout, stderr io.Writer) (Exit, error) {
	resp, err := c.do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(runID)+"/exec", req)
	if err != nil {
		return Exit{}, err
	}
	defer resp.Body.Close()
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 64<<20)
	for scanner.Scan() {
		var frame Frame
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			return Exit{}, fmt.Errorf("act serve: bad frame: %w", err)
		}
		switch {
		case frame.Exit != nil:
			return *frame.Exit, nil
		case frame.Stream == "stderr":
			_, err = stderr.Write(frame.Data)
		default:
			_, err = stdout.Write(frame.Data)
		}
		if err != nil {
			return Exit{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return Exit{}, err
	}
	return Exit{}, errors.New("act serve: the exec stream ended without an exit")
}
