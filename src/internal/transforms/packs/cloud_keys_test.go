package packs

import (
	"strings"
	"testing"
)

// The shapes behind the bare-name rules, from credentials a real transcript corpus shipped in
// clear: a query parameter, a CLI argument, a Cookie header. The near-misses are the code and
// prose a wider rule redacted there, so a widened regex or a dropped guard fails loud.
func TestCloudKeysBareNameShapes(t *testing.T) {
	hex48 := strings.Repeat("0a1b2c3d4e5f", 4)
	alnum32 := strings.Repeat("k3v9q2x7", 4)
	uuid := "1f0e2d3c-4b5a-4968-8776-655443322110"
	cases := map[string][]struct{ in, want string }{
		"token": {
			{`curl -s "https://x.workers.dev/flush?token=` + hex48 + `"`, hex48},
			{"agent -invite token=" + alnum32 + " -public-url http://127.0.0.1", alnum32},
			{`invalid value "token=` + alnum32 + `" for flag -invite`, alnum32},
			{"img.png?alt=media&token=" + uuid + `" alt=""`, uuid},
			{"workflow-csrf-token=%7B%22v%22%3A%2212ab%22%7D; next=1", "%7B%22v%22%3A%2212ab%22%7D"},
			{"TOKEN='" + alnum32 + "'", alnum32},
			{"token := bearerToken(r)", ""},
			{`if token == "" {`, ""},
			{`const token = "` + alnum32 + `"`, ""},
			{"token=undefined", ""},
			{"token=os.Getenv", ""},
			{"token=$TOKEN", ""},
			{"token=<token>", ""},
			{"token=a1b2c3d", ""},           // one short of the floor
			{"ACCESS_TOKEN=" + alnum32, ""}, // the key-name rule's, not this one's
			{"max_tokens=40960000", ""},
		},
		"cookie": {
			{"Cookie: sid=" + alnum32 + "; csrf=%7B%7D\nAccept: */*", "sid=" + alnum32 + "; csrf=%7B%7D"},
			{"set-cookie: sid=x9; Path=/; HttpOnly", "sid=x9; Path=/"},
			{"curl -H 'Cookie: a=1' https://x.example", "a=1"},
			{"send `Cookie: a=b` with every request", "a=b"},
			{"cookie: string;", ""},
			{"the Cookie: header carries it", ""},
		},
	}

	rules, err := Load(CloudKeys)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*Rule{}
	for _, r := range rules {
		byID[r.id] = r
	}
	for id, probes := range cases {
		r := byID[id]
		if r == nil {
			t.Errorf("%s: in the table, not in the pack", id)
			continue
		}
		for _, p := range probes {
			var got []string
			for _, s := range r.MatchScanned(p.in) {
				got = append(got, p.in[s.Start:s.End])
			}
			if g := strings.Join(got, " | "); g != p.want {
				t.Errorf("%s: %q: want %q, got %q", id, p.in, p.want, g)
			}
		}
	}
}
