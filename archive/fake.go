package archive

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// FakeStorage is an in-memory ArchiveStorage for unit tests. Versions are
// keyed by (key, fileID); HeadByKey returns the latest uploaded version for
// a given key.
type FakeStorage struct {
	mu      sync.Mutex
	counter atomic.Uint64
	versions map[string][]fakeVersion // keyed by key, oldest-first
	// PresignPrefix is prepended to keys by PresignDownloadURL. Default
	// "https://fake.b2.example/".
	PresignPrefix string
}

type fakeVersion struct {
	info RemoteInfo
	data []byte
}

// NewFakeStorage constructs an empty FakeStorage.
func NewFakeStorage() *FakeStorage {
	return &FakeStorage{
		versions:      make(map[string][]fakeVersion),
		PresignPrefix: "https://fake.b2.example/",
	}
}

func (s *FakeStorage) Upload(ctx context.Context, req UploadRequest) (RemoteInfo, error) {
	data, err := io.ReadAll(req.Source)
	if err != nil {
		return RemoteInfo{}, fmt.Errorf("fake upload: %w", err)
	}
	if ctx.Err() != nil {
		return RemoteInfo{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.counter.Add(1)
	info := RemoteInfo{
		FileID:      fmt.Sprintf("fake-%d", n),
		Key:         req.Key,
		Size:        int64(len(data)),
		Digest:      append([]byte(nil), req.Digest...),
		DigestAlgo:  req.DigestAlgo,
		ContentSHA1: req.ContentSHA1,
		UploadedAt:  time.Now(),
	}
	s.versions[req.Key] = append(s.versions[req.Key], fakeVersion{info: info, data: data})
	return info, nil
}

func (s *FakeStorage) HeadByKey(ctx context.Context, key string) (RemoteInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.versions[key]
	if len(vs) == 0 {
		return RemoteInfo{}, ErrNotExist
	}
	return vs[len(vs)-1].info, nil
}

func (s *FakeStorage) Download(ctx context.Context, key string, w io.Writer) error {
	s.mu.Lock()
	vs := s.versions[key]
	s.mu.Unlock()
	if len(vs) == 0 {
		return ErrNotExist
	}
	_, err := io.Copy(w, bytes.NewReader(vs[len(vs)-1].data))
	return err
}

func (s *FakeStorage) PresignDownloadURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.versions[key]; !ok {
		return "", ErrNotExist
	}
	return s.PresignPrefix + key, nil
}

// Bytes returns the bytes of the head version at key (for tests).
func (s *FakeStorage) Bytes(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vs := s.versions[key]
	if len(vs) == 0 {
		return nil, false
	}
	return append([]byte(nil), vs[len(vs)-1].data...), true
}

// VersionCount returns how many uploaded versions exist at key.
func (s *FakeStorage) VersionCount(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.versions[key])
}

// Compile-time interface check.
var _ ArchiveStorage = (*FakeStorage)(nil)
