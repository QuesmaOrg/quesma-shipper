package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"cloud.google.com/go/storage"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/iterator"
)

type gcsStore struct{ bucket *storage.BucketHandle }
type gcsSigner struct {
	bucket, account string
	signBytes       func(context.Context, []byte) ([]byte, error)
}

func newGCS(ctx context.Context, bucket, account string) (*gcsStore, UploadSigner, error) {
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load Google credentials: %w", err)
	}
	store := &gcsStore{bucket: client.Bucket(bucket)}
	if account == "" {
		return store, nil, nil
	}
	iam, err := iamcredentials.NewService(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("create IAM signing client: %w", err)
	}
	signer := &gcsSigner{bucket: bucket, account: account}
	signer.signBytes = func(ctx context.Context, payload []byte) ([]byte, error) {
		response, err := iam.Projects.ServiceAccounts.SignBlob("projects/-/serviceAccounts/"+account,
			&iamcredentials.SignBlobRequest{Payload: base64.StdEncoding.EncodeToString(payload)}).Context(ctx).Do()
		if err != nil {
			return nil, err
		}
		return base64.StdEncoding.DecodeString(response.SignedBlob)
	}
	return store, signer, nil
}

func (s *gcsStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	reader, err := s.bucket.Object(key).NewReader(ctx)
	if err != nil {
		return nil, "", mapGCSError(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, stateObjectLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > stateObjectLimit {
		return nil, "", errorsNew("state object exceeds 4 MiB")
	}
	return raw, strconv.FormatInt(reader.Attrs.Generation, 10), nil
}

func (s *gcsStore) Create(ctx context.Context, key string, raw []byte) error {
	conditions := storage.Conditions{DoesNotExist: true}
	return s.write(ctx, key, raw, &conditions)
}

func (s *gcsStore) Replace(ctx context.Context, key, version string, raw []byte) error {
	generation, err := strconv.ParseInt(version, 10, 64)
	if err != nil {
		return ErrConflict
	}
	conditions := storage.Conditions{GenerationMatch: generation}
	return s.write(ctx, key, raw, &conditions)
}

func (s *gcsStore) write(ctx context.Context, key string, raw []byte, conditions *storage.Conditions) error {
	object := s.bucket.Object(key)
	if conditions != nil {
		object = object.If(*conditions)
	}
	writer := object.NewWriter(ctx)
	writer.ContentType = "application/json"
	if _, err := writer.Write(raw); err != nil {
		_ = writer.Close()
		return mapGCSError(err)
	}
	return mapGCSError(writer.Close())
}

func (s *gcsStore) Put(ctx context.Context, key string, raw []byte) error {
	return s.write(ctx, key, raw, nil)
}

func (s *gcsStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	it := s.bucket.Objects(ctx, &storage.Query{Prefix: prefix})
	var out []ObjectInfo
	for {
		attrs, err := it.Next()
		if err == iterator.Done {
			return out, nil
		}
		if err != nil {
			return nil, mapGCSError(err)
		}
		out = append(out, ObjectInfo{Key: attrs.Name, Version: strconv.FormatInt(attrs.Generation, 10)})
	}
}

func (s *gcsStore) SourceHash(ctx context.Context, key string) (string, error) {
	attrs, err := s.bucket.Object(key).Attrs(ctx)
	if err != nil {
		return "", mapGCSError(err)
	}
	return attrs.Metadata["source-hash"], nil
}

func (s *gcsStore) VersioningEnabled(ctx context.Context) (bool, error) {
	attrs, err := s.bucket.Attrs(ctx)
	if err != nil {
		return false, mapGCSError(err)
	}
	return attrs.VersioningEnabled, nil
}

func mapGCSError(err error) error {
	if err == nil {
		return nil
	}
	if err == storage.ErrObjectNotExist {
		return ErrNotFound
	}
	if api, ok := err.(*googleapi.Error); ok {
		if api.Code == http.StatusNotFound {
			return ErrNotFound
		}
		if api.Code == http.StatusPreconditionFailed || api.Code == http.StatusConflict {
			return ErrConflict
		}
	}
	return err
}

func (s *gcsSigner) Authorize(ctx context.Context, _ InstallScope, batch UploadBatch) (TicketBatch, error) {
	out := TicketBatch{Tickets: make([]uploadTicket, 0, len(batch.Objects))}
	for _, object := range batch.Objects {
		headers := make(map[string]string, len(object.Metadata))
		for name, value := range object.Metadata {
			headers["x-goog-meta-"+strings.ToLower(name)] = value
		}
		names := make([]string, 0, len(headers))
		for name := range headers {
			names = append(names, name)
		}
		sort.Strings(names)
		signedHeaders := make([]string, 0, len(names)+1)
		for _, name := range names {
			signedHeaders = append(signedHeaders, name+":"+headers[name])
		}
		signedHeaders = append(signedHeaders, "content-length:"+strconv.FormatInt(object.Size, 10))
		rawURL, err := storage.SignedURL(s.bucket, object.Key, &storage.SignedURLOptions{GoogleAccessID: s.account,
			SignBytes: func(payload []byte) ([]byte, error) { return s.signBytes(ctx, payload) }, Method: http.MethodPut,
			Expires: batch.ExpiresAt, Headers: signedHeaders, Scheme: storage.SigningSchemeV4})
		if err != nil {
			return TicketBatch{}, fmt.Errorf("presign object %s: %s", object.ObjectID, scrubURLs(err.Error()))
		}
		out.Tickets = append(out.Tickets, uploadTicket{TicketID: object.TicketID, ObjectID: object.ObjectID, Method: http.MethodPut,
			URL: rawURL, ExpiresAt: batch.ExpiresAt, RequiredHeaders: headers, ContentLength: object.Size, ContentLengthSigned: true})
	}
	return out, nil
}
