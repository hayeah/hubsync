package hubsync

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/hayeah/hubsync/archive"
)

// Server serves the hub's HTTP API.
type Server struct {
	Hub      *Hub
	hasher   Hasher
	token    BearerToken
	listen   string
	mux      *http.ServeMux
	storage  archivePresigner // may be nil (no archive configured)
	prefix   string
	blobTTL  time.Duration
}

// archivePresigner is the narrow subset of archive.ArchiveStorage the blob
// handler needs. Using a local interface keeps the Server decoupled from
// the archive package while still letting wire inject the real type.
type archivePresigner interface {
	PresignDownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error)
}

// NewServer creates a Server and sets up routes.
func NewServer(hub *Hub, cfg ServerConfig) *Server {
	s := &Server{
		Hub:     hub,
		hasher:  cfg.Hasher,
		token:   cfg.Token,
		listen:  cfg.Listen,
		mux:     http.NewServeMux(),
		storage: cfg.Presigner,
		prefix:  cfg.Prefix,
		blobTTL: cfg.BlobTTL,
	}
	if s.blobTTL == 0 {
		s.blobTTL = time.Hour
	}
	s.routes()
	return s
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /sync/subscribe", s.auth(s.handleSubscribe))
	s.mux.HandleFunc("GET /blobs/{digest}", s.auth(s.handleBlob))
	s.mux.HandleFunc("POST /blobs/delta", s.auth(s.handleDelta))
	s.mux.HandleFunc("POST /push", s.auth(s.handlePush))
	s.mux.HandleFunc("GET /snapshots-tree/latest", s.auth(s.handleLatestSnapshotTree))
	s.mux.HandleFunc("GET /snapshots-tree/{version}", s.auth(s.handleSnapshotTree))
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
			Digest: e.Digest.Bytes(),
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

// handleBlob serves a blob by looking up the digest in hub_entry. If the
// local file is absent and the row is archive_state='unpinned', fall back
// to a 302 redirect to a presigned B2 URL (when archive is configured).
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

	// Prefer the local file if it's there.
	fullPath := filepath.Join(s.Hub.Scanner.dir, path)
	if f, err := os.Open(fullPath); err == nil {
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		io.Copy(w, f)
		return
	}

	// Local file missing — fall back to the unpinned/archive path.
	entry, found, err := s.Hub.Store.EntryLookup(path)
	if err != nil || !found || entry.ArchiveState != ArchiveStateUnpinned {
		http.Error(w, "blob not available (local missing; no archive fallback)", http.StatusNotFound)
		return
	}
	if s.storage == nil {
		http.Error(w, "blob not available (archive not configured)", http.StatusNotFound)
		return
	}

	url, err := s.storage.PresignDownloadURL(r.Context(), archive.JoinKey(s.prefix, path), s.blobTTL)
	if err != nil {
		log.Printf("presign %s: %v", path, err)
		http.Error(w, "presign failed", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// handleLatestSnapshotTree redirects to the latest snapshot-tree version.
func (s *Server) handleLatestSnapshotTree(w http.ResponseWriter, r *http.Request) {
	version, err := s.Hub.Store.LatestVersion()
	if err != nil || version == 0 {
		http.Error(w, "no snapshots available", http.StatusNotFound)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/snapshots-tree/%d", version), http.StatusFound)
}

// handleSnapshotTree serves just the hub_tree SQLite DB (no file blobs).
func (s *Server) handleSnapshotTree(w http.ResponseWriter, r *http.Request) {
	versionStr := r.PathValue("version")
	version, err := strconv.ParseInt(versionStr, 10, 64)
	if err != nil {
		http.Error(w, "invalid version", http.StatusBadRequest)
		return
	}

	dbData, err := generateClientDB(s.Hub.Store, version, s.hasher.Name())
	if err != nil {
		http.Error(w, "generate db", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/x-sqlite3")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=hub_tree_%d.db", version))
	w.Write(dbData)
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

	targetDigest := Digest(req.TargetDigest)
	if targetDigest.IsZero() {
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
