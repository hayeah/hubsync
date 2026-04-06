package hubsync

import (
	"fmt"
	"io/fs"
	"os"
	"sort"

	"github.com/hayeah/go-lstree"
)

// Scanner walks a directory and detects changes against the current tree.
type Scanner struct {
	dir     string
	ignorer lstree.Ignorer
}

// NewScanner creates a Scanner.
func NewScanner(dir string, ignorer lstree.Ignorer) *Scanner {
	return &Scanner{dir: dir, ignorer: ignorer}
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
			Mode:  0644, // default
		}

		// Try stat for mode
		fullPath := s.dir + "/" + e.Path
		if st, err := os.Lstat(fullPath); err == nil {
			info.Mode = uint32(st.Mode().Perm())
		}

		// Digest cache: reuse if mtime+size match
		if existing, ok := currentTree[e.Path]; ok {
			if existing.MTime == info.MTime && existing.Size == info.Size {
				info.Digest = existing.Digest
				result[e.Path] = info
				return nil
			}
		}

		// Read and hash
		data, err := fs.ReadFile(fsys, e.Path)
		if err != nil {
			return fmt.Errorf("read %s: %w", e.Path, err)
		}
		info.Digest = ComputeDigest(data)
		result[e.Path] = info
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan %s: %w", s.dir, err)
	}
	return result, nil
}

// Diff compares a scan result against the current tree and returns change entries.
// Creates/updates come before deletes (as required by the spec for move dedup).
func (s *Scanner) Diff(scanned map[string]FileInfo, currentTree map[string]TreeEntry) []ChangeEntry {
	var creates, deletes []ChangeEntry

	// Detect creates and updates
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

	// Detect deletes
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

	// Sort for deterministic output
	sort.Slice(creates, func(i, j int) bool { return creates[i].Path < creates[j].Path })
	sort.Slice(deletes, func(i, j int) bool { return deletes[i].Path < deletes[j].Path })

	// Creates before deletes (spec requirement for move dedup)
	return append(creates, deletes...)
}

// ProvideIgnorer creates an lstree.Ignorer for the watch directory.
func ProvideIgnorer(dir string) (lstree.Ignorer, error) {
	return lstree.NewIgnorer(dir)
}
