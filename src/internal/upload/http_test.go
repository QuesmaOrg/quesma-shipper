package upload

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const testKey = "v1/organization=acme/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301/mirror/source=claude-code-transcripts/f91631a3882c7956a9d2061b96ea38fd75bc77549b85cab26c7b2b63db21ed73.age"

type recordedPut struct {
	method  string
	path    string
	headers http.Header
	body    []byte
	length  int64
}

// objectStore records what actually arrived: the only way to check which headers were sent.
func objectStore(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *[]recordedPut, *atomic.Int64) {
	t.Helper()
	var puts []recordedPut
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		puts = append(puts, recordedPut{
			method:  r.Method,
			path:    r.URL.EscapedPath(),
			headers: r.Header.Clone(),
			body:    body,
			length:  r.ContentLength,
		})
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, &puts, &requests
}

func loopbackTarget(t *testing.T, origin string) UploadTarget {
	t.Helper()
	target, err := NewUploadTarget(TargetSpec{Origin: origin, Addressing: VirtualHosted, AllowLoopbackHTTP: true})
	if err != nil {
		t.Fatalf("build loopback target: %v", err)
	}
	return target
}

func testTicket(origin string) (PreparedUpload, Ticket) {
	body := []byte("sealed-object-bytes")
	prepared := PreparedUpload{
		ObjectID:   "trajectory-1",
		Key:        testKey,
		Body:       body,
		SourceHash: strings.Repeat("a", 64),
		Metadata: map[string]string{
			"manifest-version": "1",
			"source-id":        "claude-code-transcripts",
			"artifact-class":   "trajectory",
		},
	}
	ticket := Ticket{
		TicketID: "b1bd1a73-f16d-4a51-aac6-29f1f48b0658",
		ObjectID: "trajectory-1",
		Method:   "PUT",
		URL:      origin + "/" + canonicalPath(testKey) + "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef",
		RequiredHeaders: map[string]string{
			sourceHashHeader:              prepared.SourceHash,
			ticketIDHeader:                "b1bd1a73-f16d-4a51-aac6-29f1f48b0658",
			"x-amz-meta-manifest-version": "1",
			"x-amz-meta-source-id":        "claude-code-transcripts",
			"x-amz-meta-artifact-class":   "trajectory",
			taggingHeader:                 "class=trajectory",
		},
		ContentLength:       int64(len(body)),
		ContentLengthSigned: true,
	}
	return prepared, ticket
}

func TestUploadSendsExactlyTheAuthorizedRequest(t *testing.T) {
	server, puts, requests := objectStore(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	prepared, ticket := testTicket(server.URL)
	targets := UploadTargetList{loopbackTarget(t, server.URL)}
	if err := ValidateTicket(targets, prepared, ticket); err != nil {
		t.Fatalf("ValidateTicket: %v", err)
	}

	if err := New().Upload(context.Background(), ticket, prepared.Body); err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("object store saw %d requests, want 1", requests.Load())
	}

	got := (*puts)[0]
	if got.method != http.MethodPut {
		t.Errorf("method = %s, want PUT", got.method)
	}
	if want := "/" + canonicalPath(testKey); got.path != want {
		t.Errorf("path = %q, want %q", got.path, want)
	}
	if string(got.body) != string(prepared.Body) {
		t.Errorf("body = %q, want %q", got.body, prepared.Body)
	}
	if got.length != ticket.ContentLength {
		t.Errorf("Content-Length = %d, want %d", got.length, ticket.ContentLength)
	}
	for name, want := range ticket.RequiredHeaders {
		if have := got.headers.Get(name); have != want {
			t.Errorf("header %s = %q, want %q", name, have, want)
		}
	}
	for name := range got.headers {
		lower := strings.ToLower(name)
		if !strings.HasPrefix(lower, "x-amz-") {
			continue
		}
		if _, authorized := ticket.RequiredHeaders[lower]; !authorized {
			t.Errorf("object store saw unauthorized provider header %s", name)
		}
	}
}

func TestUploadRefusesRedirect(t *testing.T) {
	server, _, requests := objectStore(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "moved") {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Location", "/moved")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	_, ticket := testTicket(server.URL)

	err := New().Upload(context.Background(), ticket, []byte("sealed-object-bytes"))
	if !errors.Is(err, ErrRedirect) {
		t.Fatalf("error is %v, want ErrRedirect", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("object store saw %d requests, want exactly the one that was redirected", requests.Load())
	}
	assertNoURLLeak(t, err, ticket.URL)
}

func TestUploadNonOKStatusIsAFailure(t *testing.T) {
	server, _, _ := objectStore(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "<Error><Code>AccessDenied</Code>\n<Message>expired\ttoken</Message></Error>", http.StatusForbidden)
	})
	_, ticket := testTicket(server.URL)

	err := New().Upload(context.Background(), ticket, []byte("sealed-object-bytes"))
	var status *StatusError
	if !errors.As(err, &status) {
		t.Fatalf("error is %v, want *StatusError", err)
	}
	if status.Status != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", status.Status)
	}
	if strings.ContainsAny(status.Reason, "\n\t") {
		t.Fatalf("reason was not sanitized: %q", status.Reason)
	}
	if !strings.Contains(status.Reason, "AccessDenied") {
		t.Fatalf("reason lost the store's diagnostic: %q", status.Reason)
	}
	assertNoURLLeak(t, err, ticket.URL)
}

func TestUploadReaderUsesSignedLengthAndRejectsShortBody(t *testing.T) {
	server, puts, _ := objectStore(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	prepared, ticket := testTicket(server.URL)
	body := string(prepared.Body)
	prepared.Size = int64(len(body))
	prepared.Body = nil
	if err := ValidateTicket(UploadTargetList{loopbackTarget(t, server.URL)}, prepared, ticket); err != nil {
		t.Fatal(err)
	}
	if err := New().UploadReader(context.Background(), ticket, io.LimitReader(strings.NewReader(body), int64(len(body)))); err != nil {
		t.Fatal(err)
	}
	if len(*puts) != 1 || (*puts)[0].length != int64(len(body)) || string((*puts)[0].body) != body {
		t.Fatal("streamed request changed body or length")
	}
	ticket.ContentLength++
	if err := ValidateTicket(UploadTargetList{loopbackTarget(t, server.URL)}, prepared, ticket); err == nil {
		t.Fatal("accepted mismatched streamed length")
	}
	if err := New().UploadReader(context.Background(), ticket, strings.NewReader(body)); err == nil {
		t.Fatal("short streamed body reported success")
	}
}

func TestStreamedUploadHasIndependentHTTPReplayReaders(t *testing.T) {
	const body = "sealed-object-for-replay"
	section := io.NewSectionReader(strings.NewReader("prefix"+body+"suffix"), 6, int64(len(body)))
	uploader := New()
	uploader.client.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.GetBody == nil {
			t.Fatal("streamed PUT lost HTTP replay support")
		}
		prefix := make([]byte, 5)
		if _, err := io.ReadFull(request.Body, prefix); err != nil {
			t.Fatal(err)
		}
		first, err := request.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		defer first.Close()
		if _, err := io.ReadFull(first, make([]byte, 3)); err != nil {
			t.Fatal(err)
		}
		second, err := request.GetBody()
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		replay, err := io.ReadAll(second)
		if err != nil || string(replay) != body {
			t.Fatalf("HTTP replay was incomplete: %v", err)
		}
		remainder, err := io.ReadAll(request.Body)
		if err != nil || string(prefix)+string(remainder) != body {
			t.Fatalf("HTTP replay moved original offset: %v", err)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: request}, nil
	})
	ticket := Ticket{URL: "https://example.invalid/object", ContentLength: int64(len(body))}
	if err := uploader.UploadReader(context.Background(), ticket, section); err != nil {
		t.Fatal(err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }
