package sqliteread

import (
	"net/url"
	"testing"
)

func TestSessionFileURI(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"C:/Users/runner/sessions # ü.db", "file:///C:/Users/runner/sessions%20%23%20%C3%BC.db"},
		{"/tmp/sessions ?# ü.db", "file:///tmp/sessions%20%3F%23%20%C3%BC.db"},
		{"/tmp/literal%3F.db", "file:///tmp/literal%253F.db"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			got := sessionFileURI(tc.path)
			if got != tc.want {
				t.Fatalf("URI = %q, want %q", got, tc.want)
			}
			u, err := url.Parse(got)
			if err != nil {
				t.Fatal(err)
			}
			if u.Host != "" || u.RawQuery != "" || u.Fragment != "" {
				t.Fatalf("path leaked into URI components: %+v", u)
			}
		})
	}
}
