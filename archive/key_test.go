package archive

import "testing"

func TestJoinKey(t *testing.T) {
	cases := []struct {
		prefix, rel, want string
	}{
		{"", "a/b", "a/b"},
		{"", "/a/b", "a/b"},
		{"p", "a/b", "p/a/b"},
		{"p/", "a/b", "p/a/b"},
		{"p", "/a/b", "p/a/b"},
		{"p/", "/a/b", "p/a/b"},
		{"p/q", "a/b", "p/q/a/b"},
		{"p/q/", "a/b", "p/q/a/b"},
		{"b2cast", "b9a2f6", "b2cast/b9a2f6"},
		{"b2cast/", "b9a2f6", "b2cast/b9a2f6"},
		{"", "", ""},
		{"p", "", "p/"},
		{"p/", "", "p/"},
	}
	for _, c := range cases {
		got := JoinKey(c.prefix, c.rel)
		if got != c.want {
			t.Errorf("JoinKey(%q, %q) = %q; want %q", c.prefix, c.rel, got, c.want)
		}
	}
}
