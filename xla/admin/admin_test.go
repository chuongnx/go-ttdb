package admin

import "testing"

func TestBookmarkFromDescription(t *testing.T) {
	for in, want := range map[string]string{"pub_g0": "pub_g0", "  pub_g1 (pid 77) ": "pub_g1", "": ""} {
		if got := BookmarkFromDescription(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}
