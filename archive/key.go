package archive

import "strings"

// JoinKey joins a bucket prefix with a hub-relative path, producing the
// B2 object key. Normalizes both sides: trailing '/' on prefix and
// leading '/' on rel are trimmed, then rejoined with exactly one '/'
// between. Empty prefix returns rel unchanged (minus any leading '/').
//
// Use this at every call site that constructs a bucket key; do not
// concatenate prefix + rel directly. Config loaders may store
// BucketPrefix in any form ("a", "a/", ""); JoinKey is the only place
// that needs to know the rule.
func JoinKey(prefix, rel string) string {
	rel = strings.TrimPrefix(rel, "/")
	if prefix == "" {
		return rel
	}
	return strings.TrimSuffix(prefix, "/") + "/" + rel
}
