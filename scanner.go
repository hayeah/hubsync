package hubsync

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"

	"github.com/hayeah/go-lstree"
)

// Scanner walks a directory and detects changes against the current tree.
type Scanner struct {
	dir     string
	ignorer lstree.Ignorer
	hasher  Hasher
}

// NewScanner creates a Scanner from a ScannerConfig.
func NewScanner(cfg ScannerConfig) *Scanner {
	return &Scanner{dir: cfg.WatchDir, ignorer: cfg.Ignorer, hasher: cfg.Hasher}
}

// FileInfo holds stat + digest for a scanned file.
type FileInfo struct {
	Path   string
	Size   int64
	Mode   uint32
	MTime  int64
	Digest Digest
}

// ScanAll walks the directory and returns info for every file.
// It uses the current tree for digest caching: if (mtime, size) match,
// the existing digest is reused without re-reading the file.
func (s *Scanner) ScanAll(currentTree map[string]TreeEntry) (map[string]FileInfo, error) {
	fsys := os.DirFS(s.dir)
	result := make(map[string]FileInfo)

	q := lstree.Query{Stat: true}
	err := lstree.Walk(fsys, q, s.ignorer, func(e lstree.Entry) error {
		if e.IsDir {
			return nil
		}

		info := FileInfo{
			Path:  e.Path,
			Size:  e.Size,
			MTime: e.ModTime.Unix(),
			Mode:  0644,
		}

		fullPath := s.dir + "/" + e.Path
		if st, err := os.Lstat(fullPath); err == nil {
			info.Mode = uint32(st.Mode().Perm())
		}

		if existing, ok := currentTree[e.Path]; ok {
			if existing.MTime == info.MTime && existing.Size == info.Size && !existing.Digest.IsZero() {
				info.Digest = existing.Digest
				result[e.Path] = info
				return nil
			}
		}

		d, err := s.hashFile(fsys, e.Path)
		if err != nil {
			return fmt.Errorf("hash %s: %w", e.Path, err)
		}
		info.Digest = d
		result[e.Path] = info
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", s.dir, err)
	}
	return result, nil
}

func (s *Scanner) hashFile(fsys fs.FS, path string) (Digest, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := s.hasher.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return Digest(h.Sum(nil)), nil
}

// Diff compares a scan result against the current tree and returns change entries.
// Creates/updates come before deletes (as required by the spec for move dedup).
func (s *Scanner) Diff(scanned map[string]FileInfo, currentTree map[string]TreeEntry) []ChangeEntry {
	var creates, deletes []ChangeEntry

	for path, info := range scanned {
		existing, exists := currentTree[path]
		if !exists {
			creates = append(creates, ChangeEntry{
				Path:   path,
				Op:     OpCreate,
				Kind:   FileKindFile,
				Digest: info.Digest,
				Size:   info.Size,
				Mode:   info.Mode,
				MTime:  info.MTime,
			})
		} else if existing.Digest != info.Digest {
			creates = append(creates, ChangeEntry{
				Path:   path,
				Op:     OpUpdate,
				Kind:   FileKindFile,
				Digest: info.Digest,
				Size:   info.Size,
				Mode:   info.Mode,
				MTime:  info.MTime,
			})
		}
	}

	for path := range currentTree {
		if _, exists := scanned[path]; !exists {
			deletes = append(deletes, ChangeEntry{
				Path:  path,
				Op:    OpDelete,
				Kind:  FileKindFile,
				MTime: 0,
			})
		}
	}

	sort.Slice(creates, func(i, j int) bool { return creates[i].Path < creates[j].Path })
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].Path < deletes[j].Path })

	return append(creates, deletes...)
}

// ProvideIgnorer creates an lstree.Ignorer for the watch directory.
// Always excludes .hubsync/ (the hub's internal data directory).
func ProvideIgnorer(dir string) (lstree.Ignorer, error) {
	base, err := lstree.NewIgnorer(dir)
	if err != nil {
		return nil, err
	}
	return &hubsyncIgnorer{base: base}, nil
}

type hubsyncIgnorer struct {
	base lstree.Ignorer
}

func (h *hubsyncIgnorer) IsIgnored(relPath string, isDir bool) bool {
	if relPath == ".hubsync" || strings.HasPrefix(relPath, ".hubsync/") {
		return true
	}
	if h.base != nil {
		return h.base.IsIgnored(relPath, isDir)
	}
	return false
}
