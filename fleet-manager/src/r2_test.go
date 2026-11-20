package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

const testR2Account = "0123456789abcdef0123456789abcdef"

func testR2Signer(t *testing.T, jurisdiction string) UploadSigner {
	t.Helper()
	_, signer, err := newR2(testR2Account, "archive", jurisdiction, "test", "test")
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

func TestR2SignerProducesPathStyleAWSDialect(t *testing.T) {
	for jurisdiction, host := range map[string]string{"": testR2Account + ".r2.cloudflarestorage.com", "eu": testR2Account + ".eu.r2.cloudflarestorage.com"} {
		batch := providerBatch()
		batch.Objects[0].Tagging = ""
		response, err := testR2Signer(t, jurisdiction).Authorize(context.Background(), InstallScope{}, batch)
		if err != nil {
			t.Fatal(err)
		}
		ticket := response.Tickets[0]
		ticketURL, err := url.Parse(ticket.URL)
		if err != nil {
			t.Fatal(err)
		}
		if ticketURL.Host != host || !strings.HasPrefix(ticketURL.Path, "/archive/v1/organization=acme/") {
			t.Errorf("jurisdiction %q: ticket addresses %s%s", jurisdiction, ticketURL.Host, ticketURL.Path)
		}
		if !ticket.ContentLengthSigned {
			t.Error("R2 ticket does not sign content-length")
		}
		if err := validateTicketHeaders(batch.Objects[0], ticket.RequiredHeaders); err != nil {
			t.Fatal(err)
		}
	}
}

func TestR2SignerRefusesTags(t *testing.T) {
	_, err := testR2Signer(t, "").Authorize(context.Background(), InstallScope{}, providerBatch())
	if err == nil || !strings.Contains(err.Error(), "o-1") {
		t.Fatalf("tagged object was signed for R2: %v", err)
	}
}

func TestR2PutSendsNoTagsAndVersioningIsNotQueried(t *testing.T) {
	var mu sync.Mutex
	var requests []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r)
		mu.Unlock()
		w.Header().Set("ETag", `"etag"`)
	}))
	defer server.Close()
	client := awss3.New(awss3.Options{Region: "auto", BaseEndpoint: aws.String(server.URL), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")})
	store := &r2Store{&s3Store{bucket: "archive", client: client}}
	ctx := context.Background()
	if err := store.Put(ctx, "v1/organization=acme/control/seen/x.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := store.Create(ctx, "v1/organization=acme/control/config.json", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	enabled, err := store.VersioningEnabled(ctx)
	if err != nil || enabled {
		t.Fatalf("versioning = %t, %v", enabled, err)
	}
	if len(requests) != 2 {
		t.Fatalf("got %d requests, want Put and Create only", len(requests))
	}
	if put := requests[0]; put.Method != http.MethodPut || put.URL.Path != "/archive/v1/organization=acme/control/seen/x.json" || put.Header.Get("x-amz-tagging") != "" {
		t.Errorf("Put sent %s %s tagging=%q", put.Method, put.URL.Path, put.Header.Get("x-amz-tagging"))
	}
	if create := requests[1]; create.Header.Get("If-None-Match") != "*" {
		t.Errorf("Create lost its condition: If-None-Match=%q", create.Header.Get("If-None-Match"))
	}
}

func TestR2RefusesInvalidTargets(t *testing.T) {
	for name, args := range map[string][5]string{
		"uppercase account": {strings.ToUpper(testR2Account), "b", "", "k", "s"},
		"short account":     {"0123abcd", "b", "", "k", "s"},
		"jurisdiction":      {testR2Account, "b", "us", "k", "s"},
		"missing key id":    {testR2Account, "b", "", "", "s"},
		"missing secret":    {testR2Account, "b", "", "k", ""},
	} {
		if _, _, err := newR2(args[0], args[1], args[2], args[3], args[4]); err == nil || strings.Contains(err.Error(), "\n") {
			t.Errorf("%s: want a one-line error, got %v", name, err)
		}
	}
}

func TestOpenTargetWaivesVersioningOnlyForR2(t *testing.T) {
	t.Setenv("FLEET_MANAGER_R2_ACCESS_KEY_ID", "k")
	t.Setenv("FLEET_MANAGER_R2_SECRET_ACCESS_KEY", "s")
	manager, _, err := openTarget(context.Background(), &targetOptions{provider: "r2", bucket: "archive", account: testR2Account})
	if err != nil {
		t.Fatal(err)
	}
	org, err := manager.ForOrganization("acme")
	if err != nil {
		t.Fatal(err)
	}
	if !manager.allowUnversioned || !org.allowUnversioned {
		t.Fatal("r2 manager still requires versioning")
	}
	// Loading AWS config makes no request, so this stays offline; a stray profile must not interfere.
	t.Setenv("AWS_CONFIG_FILE", os.DevNull)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", os.DevNull)
	t.Setenv("AWS_PROFILE", "")
	versioned, _, err := openTarget(context.Background(), &targetOptions{provider: "aws", bucket: "archive", region: "eu-central-1"})
	if err != nil {
		t.Fatal(err)
	}
	if versioned.allowUnversioned {
		t.Fatal("aws manager waives versioning")
	}
}
