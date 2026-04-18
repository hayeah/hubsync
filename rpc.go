package hubsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Fixed filenames under .hubsync/.
const (
	RPCSocketName = "serve.sock"
	HubDBName     = "hub.db"
	HubLockName   = "hub.lock"
)

// RPCSocketPath returns the absolute path to the hub's RPC socket.
func RPCSocketPath(hubDir string) string {
	return filepath.Join(hubDir, ".hubsync", RPCSocketName)
}

// HubDBPath returns the absolute path to the hub's SQLite DB.
func HubDBPath(hubDir string) string {
	return filepath.Join(hubDir, ".hubsync", HubDBName)
}

// HubLockPath returns the absolute path to the hub's one-shot lock file.
func HubLockPath(hubDir string) string {
	return filepath.Join(hubDir, ".hubsync", HubLockName)
}

// RPCServer accepts local pin/unpin/ls/status requests from the CLI over a
// unix-domain socket. Runs inside serve alongside the archive worker.
type RPCServer struct {
	Reconciler *Reconciler
	Store      *HubStore
	Token      BearerToken
	Socket     string // absolute path; created with 0600 perms
}

// ListenAndServe binds the socket and serves until ctx is cancelled.
func (s *RPCServer) ListenAndServe(ctx context.Context) error {
	if err := os.MkdirAll(filepath.Dir(s.Socket), 0755); err != nil {
		return err
	}
	// Stale socket from a previous crashed serve is cleaned up here. If
	// two serves start in the same dir they'll race the bind; that's
	// an operator-visible error via the second bind failing.
	_ = os.Remove(s.Socket)
	ln, err := net.Listen("unix", s.Socket)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.Socket, err)
	}
	if err := os.Chmod(s.Socket, 0600); err != nil {
		ln.Close()
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /rpc/pin", s.auth(s.handlePin))
	mux.HandleFunc("POST /rpc/unpin", s.auth(s.handleUnpin))
	mux.HandleFunc("GET /rpc/ls", s.auth(s.handleLs))
	mux.HandleFunc("GET /rpc/status", s.auth(s.handleStatus))
	srv := &http.Server{Handler: mux}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		os.Remove(s.Socket)
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *RPCServer) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Token != "" {
			want := "Bearer " + string(s.Token)
			if r.Header.Get("Authorization") != want {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

// ---- pin / unpin ------------------------------------------------------

// PinRequest / UnpinRequest share wire shape.
type PinRequest struct {
	Globs []string `json:"globs"`
	Dry   bool     `json:"dry"`
}

// PinResponse carries per-path results. The CLI prints them streaming-style
// but the wire body is a single JSON object for simplicity.
type PinResponse struct {
	Results []PinResult `json:"results"`
}

type PinResult struct {
	Path          string   `json:"path"`
	StartingState string   `json:"starting_state"`
	Steps         []string `json:"steps"`
	Error         string   `json:"error,omitempty"`
	Dry           bool     `json:"dry,omitempty"`
}

func (s *RPCServer) handlePin(w http.ResponseWriter, r *http.Request) {
	s.handleReconcile(w, r, TargetArchived)
}

func (s *RPCServer) handleUnpin(w http.ResponseWriter, r *http.Request) {
	s.handleReconcile(w, r, TargetUnpinned)
}

func (s *RPCServer) handleReconcile(w http.ResponseWriter, r *http.Request, target TargetState) {
	if s.Reconciler == nil {
		http.Error(w, "archive not configured ([archive] section missing from .hubsync/config.toml)", http.StatusServiceUnavailable)
		return
	}
	var req PinRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(req.Globs) == 0 {
		http.Error(w, "globs required", http.StatusBadRequest)
		return
	}
	resp, err := RunReconcile(r.Context(), s.Reconciler, s.Store, req, target)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrNoMatches) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// ErrNoMatches is returned by RunReconcile when the globs match no paths.
var ErrNoMatches = errors.New("no matches")

// RunReconcile is the in-process pin/unpin driver, shared by the RPC handler
// and the one-shot CLI path. Expands globs, plans + (unless dry) applies each
// path's plan, and returns a PinResponse with per-path results.
func RunReconcile(ctx context.Context, rec *Reconciler, store *HubStore, req PinRequest, target TargetState) (PinResponse, error) {
	matches, err := store.MatchGlobs(req.Globs)
	if err != nil {
		return PinResponse{}, err
	}
	if len(matches) == 0 {
		return PinResponse{}, ErrNoMatches
	}
	resp := PinResponse{}
	for _, path := range matches {
		var plan Plan
		var planErr error
		switch target {
		case TargetArchived:
			plan, planErr = rec.PlanPin(path)
		case TargetUnpinned:
			plan, planErr = rec.PlanUnpin(path)
		}
		res := PinResult{Path: path, StartingState: plan.StartingState, Dry: req.Dry}
		for _, st := range plan.Steps {
			res.Steps = append(res.Steps, string(st))
		}
		if planErr != nil {
			res.Error = planErr.Error()
		} else if !req.Dry {
			if err := rec.Apply(ctx, plan); err != nil {
				res.Error = err.Error()
			}
		}
		resp.Results = append(resp.Results, res)
	}
	return resp, nil
}

// ---- ls ---------------------------------------------------------------

// LsResponse is the payload for `/rpc/ls` and the no-serve `LocalLs`
// fallback. Entries are sorted by path; each entry is a HubEntry
// serialized through its JSON tags (one key per SQL column).
type LsResponse struct {
	Entries []HubEntry `json:"entries"`
}

// LocalLs reads the tree directly from the store and returns the same
// payload the /rpc/ls handler emits. Used by the CLI's no-serve fallback
// and by the RPC handler itself.
func LocalLs(store *HubStore) (LsResponse, error) {
	entries, err := store.EntrySnapshot()
	if err != nil {
		return LsResponse{}, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return LsResponse{Entries: entries}, nil
}

func (s *RPCServer) handleLs(w http.ResponseWriter, r *http.Request) {
	out, err := LocalLs(s.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---- status -----------------------------------------------------------

type StatusResponse struct {
	Tree     TreeCounts     `json:"tree"`
	Archive  ArchiveCounts  `json:"archive"`
	Dirty    ArchiveCounts  `json:"dirty"`
	Unpinned ArchiveCounts  `json:"unpinned"`
	Null     ArchiveCounts  `json:"null"`
}

type TreeCounts struct {
	Files int64 `json:"files"`
	Dirs  int64 `json:"dirs"`
}

type ArchiveCounts struct {
	Count int64 `json:"count"`
	Bytes int64 `json:"bytes"`
}

// LocalStatus computes counts directly from the store. Used by the CLI's
// no-serve fallback and by the RPC handler itself.
func LocalStatus(store *HubStore) (StatusResponse, error) {
	var out StatusResponse
	entries, err := store.EntrySnapshot()
	if err != nil {
		return out, err
	}
	for _, e := range entries {
		switch e.Kind {
		case FileKindFile:
			out.Tree.Files++
		case FileKindDirectory:
			out.Tree.Dirs++
		}
		if e.Kind != FileKindFile {
			continue
		}
		switch e.ArchiveState {
		case ArchiveStateArchived:
			out.Archive.Count++
			out.Archive.Bytes += e.Size
		case ArchiveStateDirty:
			out.Dirty.Count++
			out.Dirty.Bytes += e.Size
		case ArchiveStateUnpinned:
			out.Unpinned.Count++
			out.Unpinned.Bytes += e.Size
		case "":
			out.Null.Count++
			out.Null.Bytes += e.Size
		}
	}
	return out, nil
}

func (s *RPCServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	out, err := LocalStatus(s.Store)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---- client helpers --------------------------------------------------

// RPCClient is a thin helper used by the CLI to call an already-running
// serve process. Keeps JSON marshaling local to this file.
type RPCClient struct {
	Socket string
	Token  string
	http   *http.Client
}

// NewRPCClient constructs a client that talks to socket (owner-only).
func NewRPCClient(socket, token string) *RPCClient {
	return &RPCClient{
		Socket: socket,
		Token:  token,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socket)
				},
			},
		},
	}
}

// Pin / Unpin / Ls / Status perform the corresponding RPC round-trip.
func (c *RPCClient) Pin(ctx context.Context, req PinRequest) (*PinResponse, error) {
	return callRPC[PinResponse](ctx, c, "POST", "/rpc/pin", req)
}

func (c *RPCClient) Unpin(ctx context.Context, req PinRequest) (*PinResponse, error) {
	return callRPC[PinResponse](ctx, c, "POST", "/rpc/unpin", req)
}

func (c *RPCClient) Ls(ctx context.Context) (*LsResponse, error) {
	return callRPC[LsResponse](ctx, c, "GET", "/rpc/ls", nil)
}

func (c *RPCClient) Status(ctx context.Context) (*StatusResponse, error) {
	return callRPC[StatusResponse](ctx, c, "GET", "/rpc/status", nil)
}

func callRPC[T any](ctx context.Context, c *RPCClient, method, path string, body any) (*T, error) {
	var reqBody *strings.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reqBody = strings.NewReader(string(data))
	}
	var r *http.Request
	var err error
	if reqBody != nil {
		r, err = http.NewRequestWithContext(ctx, method, "http://unix"+path, reqBody)
	} else {
		r, err = http.NewRequestWithContext(ctx, method, "http://unix"+path, nil)
	}
	if err != nil {
		return nil, err
	}
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		r.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") || strings.Contains(err.Error(), "connection refused") {
			return nil, fmt.Errorf("no hubsync serve running at %s; start it with `hubsync serve`", c.Socket)
		}
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var buf [512]byte
		n, _ := resp.Body.Read(buf[:])
		return nil, fmt.Errorf("rpc %s %s: %s (%s)", method, path, resp.Status, strings.TrimSpace(string(buf[:n])))
	}
	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---- helpers ---------------------------------------------------------

// StartRPCServer is a convenience for serve cmd wiring.
func StartRPCServer(ctx context.Context, s *RPCServer) error {
	log.Printf("rpc listening on %s", s.Socket)
	return s.ListenAndServe(ctx)
}
