package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"google.golang.org/api/googleapi"
)

func providerBatch() UploadBatch {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	return UploadBatch{IssuedAt: now, ExpiresAt: now.Add(5 * time.Minute), Objects: []UploadObjectRequest{{ObjectID: "o-1", TicketID: "t-1",
		Key: "v1/organization=acme/install=3f2504e0-4f89-41d3-9a0c-0305e82c3301/state/heartbeat.json.age", Size: 123,
		Metadata: map[string]string{"source-hash": strings.Repeat("a", 64), "ticket-id": "t-1"}, Tagging: "class=context"}}}
}

func TestS3SignerProducesAWSHeaderDialect(t *testing.T) {
	client := awss3.NewFromConfig(aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")})
	response, err := (&s3Signer{bucket: "archive", client: client}).Authorize(context.Background(), InstallScope{}, providerBatch())
	if err != nil {
		t.Fatal(err)
	}
	ticket := response.Tickets[0]
	if ticket.RequiredHeaders["x-amz-meta-source-hash"] == "" || ticket.RequiredHeaders["x-amz-tagging"] != "class=context" || !ticket.ContentLengthSigned {
		t.Fatalf("invalid S3 ticket: %#v", ticket)
	}
}

func TestGCSErrorMapsWrappedNotFound(t *testing.T) {
	api := &googleapi.Error{Code: http.StatusNotFound}
	for _, err := range []error{storage.ErrObjectNotExist, fmt.Errorf("%w: %w", storage.ErrObjectNotExist, api), api, fmt.Errorf("attrs: %w", api)} {
		if got := mapGCSError(err); got != ErrNotFound {
			t.Errorf("mapGCSError(%v) = %v, want ErrNotFound", err, got)
		}
	}
	if got := mapGCSError(fmt.Errorf("attrs: %w", &googleapi.Error{Code: http.StatusPreconditionFailed})); got != ErrConflict {
		t.Errorf("wrapped 412 = %v, want ErrConflict", got)
	}
	if denied := (&googleapi.Error{Code: http.StatusForbidden}); mapGCSError(denied) != error(denied) {
		t.Error("a denied read must stay an error, never read as absent")
	}
}

func TestGCSSignerProducesGoogleHeaderDialect(t *testing.T) {
	signer := &gcsSigner{bucket: "archive", account: "signer@example.iam.gserviceaccount.com", signBytes: func(context.Context, []byte) ([]byte, error) { return []byte("signature"), nil }}
	response, err := signer.Authorize(context.Background(), InstallScope{}, providerBatch())
	if err != nil {
		t.Fatal(err)
	}
	ticket := response.Tickets[0]
	if ticket.RequiredHeaders["x-goog-meta-source-hash"] == "" || ticket.RequiredHeaders["x-amz-tagging"] != "" || !ticket.ContentLengthSigned {
		t.Fatalf("invalid GCS ticket: %#v", ticket)
	}
}

type fakeDelegation struct{ calls int }

func (f *fakeDelegation) GetUserDelegationCredential(context.Context, service.KeyInfo, *service.GetUserDelegationCredentialOptions) (*service.UserDelegationCredential, error) {
	f.calls++
	return &service.UserDelegationCredential{}, nil
}

func TestAzureSignerProducesAzureHeaderDialect(t *testing.T) {
	var signed sas.BlobSignatureValues
	delegation := &fakeDelegation{}
	signer := &azureSigner{account: "archive", container: "trajectories", delegation: delegation, clock: func() time.Time { return providerBatch().IssuedAt },
		signSAS: func(values sas.BlobSignatureValues, _ *service.UserDelegationCredential) (string, error) {
			signed = values
			return "sv=test&sig=secret", nil
		}}
	response, err := signer.Authorize(context.Background(), InstallScope{}, providerBatch())
	if err != nil {
		t.Fatal(err)
	}
	ticket := response.Tickets[0]
	if ticket.RequiredHeaders["x-ms-meta-source_hash"] == "" || ticket.RequiredHeaders["x-ms-tags"] != "class=context" || ticket.RequiredHeaders["x-ms-blob-type"] != "BlockBlob" {
		t.Fatalf("invalid Azure ticket: %#v", ticket)
	}
	if signed.SignedRequestHeaders["content-length"] != strconv.FormatInt(ticket.ContentLength, 10) {
		t.Fatal("Azure SAS did not bind content length")
	}
	if _, err := signer.Authorize(context.Background(), InstallScope{}, providerBatch()); err != nil || delegation.calls != 1 {
		t.Fatalf("delegation cache calls=%d err=%v", delegation.calls, err)
	}
}

func TestUploadSignersOmitClassTags(t *testing.T) {
	client := awss3.NewFromConfig(aws.Config{Region: "eu-central-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")})
	signers := map[string]UploadSigner{
		"s3": &s3Signer{bucket: "archive", client: client},
		"gcs": &gcsSigner{bucket: "archive", account: "signer@example.iam.gserviceaccount.com",
			signBytes: func(context.Context, []byte) ([]byte, error) { return []byte("signature"), nil }},
		"azure": &azureSigner{account: "archive", container: "trajectories", delegation: &fakeDelegation{},
			clock: func() time.Time { return providerBatch().IssuedAt },
			signSAS: func(values sas.BlobSignatureValues, _ *service.UserDelegationCredential) (string, error) {
				if strings.Contains(values.Permissions, "t") {
					return "", fmt.Errorf("untagged upload grants Azure tag permission")
				}
				return "sv=test&sig=secret", nil
			}},
	}
	install := InstallRecord{Organization: "acme", InstallID: "3f2504e0-4f89-41d3-9a0c-0305e82c3301"}
	for _, fixture := range []string{"request.json", "request-heartbeat.json"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := readProtocol("fixtures/v2/uploads-authorize", fixture)
			if err != nil {
				t.Fatal(err)
			}
			var req uploadAuthorizeRequest
			if err := strictDecode(raw, &req); err != nil {
				t.Fatal(err)
			}
			objects, err := validateUploadRequest(req, install, req.IssuedAt)
			if err != nil {
				t.Fatal(err)
			}
			for i := range objects {
				objects[i].TicketID = "8403c1de-6940-4e35-a19b-5c91c45fc379"
				objects[i].Metadata["ticket-id"] = objects[i].TicketID
			}
			batch := UploadBatch{IssuedAt: req.IssuedAt, ExpiresAt: req.IssuedAt.Add(uploadTicketLifetime), Objects: objects}
			for name, signer := range signers {
				t.Run(name, func(t *testing.T) {
					response, err := signer.Authorize(context.Background(), InstallScope{install.Organization, install.InstallID}, batch)
					if err != nil {
						t.Fatal(err)
					}
					if len(response.Tickets) != len(objects) {
						t.Fatalf("got %d tickets for %d objects", len(response.Tickets), len(objects))
					}
					for i, ticket := range response.Tickets {
						if err := validateTicketHeaders(objects[i], ticket.RequiredHeaders); err != nil {
							t.Fatal(err)
						}
						for _, header := range []string{"x-amz-tagging", "x-ms-tags"} {
							if _, ok := ticket.RequiredHeaders[header]; ok {
								t.Errorf("upload ticket carries %s", header)
							}
						}
					}
					validateSchema(t, "v2/uploads-authorize-response.schema.json", uploadAuthorizeResponse{Tickets: response.Tickets})
				})
			}
		})
	}
}
