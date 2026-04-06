package hubsync

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"google.golang.org/protobuf/proto"
)

// Server serves the hub's HTTP API.
type Server struct {
	Hub    *Hub
	token  BearerToken
	listen string
	mux    *http.ServeMux
}

// NewServer creates a Server and sets up routes.
func NewServer(hub *Hub, token BearerToken, listen string) *Server {
	s := &Server{Hub: hub, token: token, listen: listen, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /sync/subscribe", s.auth(s.handleSubscribe))
	s.mux.HandleFunc("GET /blobs/{digest}", s.auth(s.handleBlob))
	s.mux.HandleFunc("POST /blobs/delta", s.auth(s.handleDelta))
	s.mux.HandleFunc("GET /snapshots/latest", s.auth(s.handleLatestSnapshot))
	s.mux.HandleFunc("GET /snapshots/{version}", s.auth(s.handleSnapshot))
	s.mux.HandleFunc("POST /push", s.auth(s.handlePush))
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return AuthMiddleware(s.token, next)
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// ListenAndServe starts the HTTP server.
func (s *Server) ListenAndServe() error {
	log.Printf("hub listening on %s", s.listen)
	return http.ListenAndServe(s.listen, s)
}

// handleSubscribe streams change events to the client as length-prefixed protobuf.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	sinceStr := r.URL.Query().Get("since")
	var since int64
	if sinceStr != "" {
		var err error
		since, err = strconv.ParseInt(sinceStr, 10, 64)
		if err != nil {
			http.Error(w, "invalid since parameter", http.StatusBadRequest)
			return
		}
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Subscribe first, then backfill — ensures no gap.
	sub := s.Hub.Broadcaster.Subscribe()
	defer s.Hub.Broadcaster.Unsubscribe(sub)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	// Backfill from change log
	entries, err := s.Hub.Store.ChangesSince(since)
	if err != nil {
		log.Printf("backfill error: %v", err)
		return
	}

	lastVersion := since
	for _, e := range entries {
		if err := s.writeEvent(w, e); err != nil {
			log.Printf("write backfill event: %v", err)
			return
		}
		lastVersion = e.Version
	}
	flusher.Flush()

	// Stream live events
	for {
		select {
		case <-r.Context().Done():
			return
		case entry := <-sub:
			// Deduplicate: skip events already sent in backfill
			if entry.Version <= lastVersion {
				continue
			}
			if err := s.writeEvent(w, entry); err != nil {
				log.Printf("write live event: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}

// writeEvent converts a ChangeEntry to a protobuf SyncEvent and writes it.
func (s *Server) writeEvent(w io.Writer, e ChangeEntry) error {
	ev := &SyncEvent{
		Version: uint64(e.Version),
		Path:    e.Path,
	}

	if e.Op == OpDelete {
		ev.Event = &SyncEvent_Delete{Delete: &FileDelete{}}
	} else {
		change := &FileChange{
			Kind:   EntryKind(e.Kind),
			Digest: e.Digest[:],
			Size:   uint64(e.Size),
			Mode:   e.Mode,
			Mtime:  e.MTime,
		}

		// Inline small files
		if e.Size > 0 && e.Size <= InlineThreshold {
			path := filepath.Join(s.Hub.Scanner.dir, e.Path)
			if data, err := os.ReadFile(path); err == nil {
				change.Data = data
			}
		}

		ev.Event = &SyncEvent_Change{Change: change}
	}

	return WriteLengthPrefixed(w, ev)
}

// handleBlob serves a blob by looking up the digest in the current tree.
func (s *Server) handleBlob(w http.ResponseWriter, r *http.Request) {
	digestHex := r.PathValue("digest")
	digest, err := ParseDigest(digestHex)
	if err != nil {
		http.Error(w, "invalid digest", http.StatusBadRequest)
		return
	}

	path, ok := s.Hub.Store.PathByDigest(digest)
	if !ok {
		http.Error(w, "blob not found", http.StatusNotFound)
		return
	}

	fullPath := filepath.Join(s.Hub.Scanner.dir, path)
	f, err := os.Open(fullPath)
	if err != nil {
		http.Error(w, "file not found", http.StatusNotFound)
		return
	}
	defer f.Close()

	w.Header().Set("Content-Type", "application/octet-stream")
	io.Copy(w, f)
}

// handleLatestSnapshot redirects to the latest snapshot version.
func (s *Server) handleLatestSnapshot(w http.ResponseWriter, r *http.Request) {
	version, err := s.Hub.Store.LatestVersion()
	if err != nil || version == 0 {
		http.Error(w, "no snapshots available", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/snapshots/%d", version), http.StatusFound)
}

// handleSnapshot serves a snapshot tarball.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	versionStr := r.PathValue("version")
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/zstd")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%d.tar.zst", version))

	if err := GenerateSnapshot(s.Hub.Store, s.Hub.Scanner.dir, version, w); err != nil {
		log.Printf("snapshot generation error: %v", err)
		// Headers already sent, can't return error to client
	}
}

// handlePush processes a PushRequest and returns a PushResponse.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var req PushRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid protobuf", http.StatusBadRequest)
		return
	}

	resp := s.Hub.ProcessPush(&req)

	respData, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(respData)
}

// handleDelta receives a DeltaRequest (block signatures from client's old version)
// and responds with a DeltaResponse (delta ops to reconstruct the target file).
func (s *Server) handleDelta(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	var req DeltaRequest
	if err := proto.Unmarshal(body, &req); err != nil {
		http.Error(w, "invalid protobuf", http.StatusBadRequest)
		return
	}

	targetDigest, err := ParseDigest(fmt.Sprintf("%x", req.TargetDigest))
	if err != nil {
		http.Error(w, "invalid target digest", http.StatusBadRequest)
		return
	}

	// Find the target file on disk
	targetPath, ok := s.Hub.Store.PathByDigest(targetDigest)
	if !ok {
		http.Error(w, "target not found", http.StatusNotFound)
		return
	}

	targetData, err := os.ReadFile(filepath.Join(s.Hub.Scanner.dir, targetPath))
	if err != nil {
		http.Error(w, "read target file", http.StatusInternalServerError)
		return
	}

	// Convert proto signatures to internal format
	sigs := make([]BlockSig, len(req.Signature))
	for i, ps := range req.Signature {
		sigs[i] = BlockSig{
			Index:    ps.Index,
			WeakHash: ps.WeakHash,
		}
		if len(ps.StrongHash) == 32 {
			copy(sigs[i].StrongHash[:], ps.StrongHash)
		}
	}

	// Compute delta
	blockSize := int(req.BlockSize)
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}
	delta := ComputeDelta(targetData, sigs, blockSize)

	// Send response
	resp := delta.ToProto(req.TargetDigest)
	respData, err := proto.Marshal(resp)
	if err != nil {
		http.Error(w, "marshal response", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(respData)
}
