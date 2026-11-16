package app

// The bucket probe: one heartbeat ticket, validated like an upload and never spent, read back to
// the bucket it names without its query ever reaching an error.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/QuesmaOrg/quesma-shipper/internal/controlplane"
	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

const probeKey = "v1/organization=acme/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301/state/heartbeat.json.age"

// escapedKey is the key as a presigner spells it in a ticket path.
var escapedKey = strings.ReplaceAll(probeKey, "=", "%3D")

func TestProbeSinkUsesEnrollmentWithoutUsableConfigCache(t *testing.T) {
	for _, cached := range []string{"", "drain_deadline: forever\n"} {
		t.Run(fmt.Sprintf("cache=%q", cached), func(t *testing.T) {
			var wantKey string
			paths := enrolledConfigServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v2/uploads/authorize" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				var req controlplane.AuthorizeRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					http.Error(w, "invalid request", http.StatusBadRequest)
					return
				}
				if len(req.Objects) != 1 || req.Objects[0].Key != wantKey {
					http.Error(w, "key is outside this install's root", http.StatusBadRequest)
					return
				}
				obj := req.Objects[0]
				const ticketID = "b1bd1a73-f16d-4a51-aac6-29f1f48b0658"
				_ = json.NewEncoder(w).Encode(controlplane.AuthorizeResponse{Tickets: []controlplane.Ticket{{
					TicketID: ticketID, ObjectID: obj.ObjectID, Method: http.MethodPut,
					URL:           "https://archive.s3.eu-central-1.amazonaws.com/" + strings.ReplaceAll(obj.Key, "=", "%3D"),
					ContentLength: obj.Size, ContentLengthSigned: true,
					RequiredHeaders: map[string]string{
						"x-amz-meta-source-hash": obj.SourceHash,
						"x-amz-meta-ticket-id":   ticketID,
						"x-amz-meta-kind":        "heartbeat",
					},
				}}})
			})
			_, enrollment := enrolledIdentity(t, paths.StateDir)
			wantKey = "v1/organization=acme/install=" + enrollment.InstallID + "/state/heartbeat.json.age"
			if cached != "" {
				if err := controlplane.SaveCache(paths.StateDir, controlplane.Cached{Config: []byte(cached)}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := ProbeSink(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Bucket != "archive" || got.Prefix != "v1/organization=acme/" {
				t.Errorf("got %+v", got)
			}
		})
	}
}

// probePlane answers the authorization with one heartbeat ticket at path, or already_present.
func probePlane(t *testing.T, bodies *[]string, path string, present bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("the probe sent %s %s", r.Method, r.URL.Path)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if present {
			fmt.Fprint(w, `{"tickets":[{"ticket_id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0658","object_id":"heartbeat","already_present":true}]}`)
			return
		}
		fmt.Fprintf(w, `{"tickets":[{"ticket_id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0658","object_id":"heartbeat","method":"PUT",`+
			`"url":"http://%s%s?X-Amz-Signature=secret","expires_at":"2099-01-01T00:00:00Z",`+
			`"required_headers":{"x-amz-meta-source-hash":"%s","x-amz-meta-ticket-id":"b1bd1a73-f16d-4a51-aac6-29f1f48b0658",`+
			`"x-amz-meta-kind":"heartbeat"},"content_length":5,"content_length_signed":true}]}`,
			r.Host, path, transforms.Hash([]byte("probe")))
	}
}

func TestProbeSinkReadsTheBucketWithoutAPut(t *testing.T) {
	var bodies []string
	p := portAgainst(t, probePlane(t, &bodies, "/b/"+escapedKey, false))
	got, err := p.probeSink(context.Background(), "acme", "3f2504e0-4f89-41d3-9a0c-0305e82c3301")
	if err != nil {
		t.Fatal(err)
	}
	if got.Bucket != "b" || got.Provider != "" || got.Region != "" || got.Prefix != "v1/organization=acme/" ||
		!strings.HasPrefix(got.Origin, "http://127.0.0.1:") {
		t.Errorf("got %+v", got)
	}
	if len(bodies) != 1 || !strings.Contains(bodies[0], `"key":"`+probeKey+`"`) || !strings.Contains(bodies[0], `"kind":"heartbeat"`) {
		t.Errorf("the probe did not ask for the heartbeat key alone: %q", bodies)
	}
}

func TestProbeSinkRefusesAnUnusableTicket(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		present    bool
		want       string
	}{
		{"another install's key", "/b/" + strings.Replace(escapedKey, "acme", "evil", 1), false, "does not name the prepared key"},
		{"no URL", "", true, "already_present"},
	} {
		var bodies []string
		p := portAgainst(t, probePlane(t, &bodies, tc.path, tc.present))
		_, err := p.probeSink(context.Background(), "acme", "3f2504e0-4f89-41d3-9a0c-0305e82c3301")
		if err == nil || !strings.Contains(err.Error(), tc.want) || strings.Contains(err.Error(), "secret") {
			t.Errorf("%s: got %v, want an error containing %q", tc.name, err, tc.want)
		}
	}
}

func TestSinkFromURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want Sink
		err  string
	}{
		{url: "https://quesma-agent-statement-prod-archive.s3.eu-central-1.amazonaws.com/" + escapedKey,
			want: Sink{Provider: "s3", Bucket: "quesma-agent-statement-prod-archive", Region: "eu-central-1"}},
		{url: "https://s3.us-west-2.amazonaws.com/my.dotted.bucket/" + escapedKey,
			want: Sink{Provider: "s3", Bucket: "my.dotted.bucket", Region: "us-west-2"}},
		{url: "https://legacy.s3-eu-west-1.amazonaws.com/" + escapedKey,
			want: Sink{Provider: "s3", Bucket: "legacy", Region: "eu-west-1"}},
		{url: "https://dual.s3.dualstack.us-gov-west-1.amazonaws.com/" + escapedKey,
			want: Sink{Provider: "s3", Bucket: "dual", Region: "us-gov-west-1"}},
		{url: "https://global.s3.amazonaws.com/" + escapedKey,
			want: Sink{Provider: "s3", Bucket: "global"}},
		{url: "https://storage.googleapis.com/proj-trajectories/" + escapedKey,
			want: Sink{Provider: "gcs", Bucket: "proj-trajectories"}},
		{url: "https://acct.blob.core.windows.net/trajectories/" + escapedKey,
			want: Sink{Provider: "azure", Bucket: "trajectories"}},
		{url: "http://127.0.0.1:9000/minio/" + escapedKey, want: Sink{Bucket: "minio"}},
		{url: "https://example.com/" + escapedKey, err: "names no bucket"},
		{url: "https://b.s3.eu-central-1.amazonaws.com/b/" + escapedKey, err: "both host and path"},
		{url: "https://example.com/a/b/" + escapedKey, err: "not one bucket"},
		{url: "https://b.s3.eu-central-1.amazonaws.com/other", err: "does not end in the heartbeat key"},
	} {
		got, err := sinkFromURL(tc.url+"?X-Amz-Signature=secret", probeKey, "p/")
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) || strings.Contains(err.Error(), "secret") {
				t.Errorf("%s: got %+v, %v; want an error containing %q", tc.url, got, err, tc.err)
			}
			continue
		}
		u, _ := url.Parse(tc.url)
		tc.want.Origin, tc.want.Prefix = u.Scheme+"://"+u.Host, "p/"
		if err != nil || got != tc.want {
			t.Errorf("%s: got %+v, %v; want %+v", tc.url, got, err, tc.want)
		}
	}
}
