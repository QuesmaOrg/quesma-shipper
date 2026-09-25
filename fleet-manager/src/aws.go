package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type s3Store struct {
	bucket string
	client *awss3.Client
}

func newS3(ctx context.Context, bucket, region string) (*s3Store, UploadSigner, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
	if err != nil {
		return nil, nil, fmt.Errorf("load AWS credentials: %w", err)
	}
	client := awss3.NewFromConfig(cfg)
	store := &s3Store{bucket: bucket, client: client}
	return store, &s3Signer{bucket: bucket, client: client}, nil
}

func (s *s3Store) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObject(ctx, &awss3.GetObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return nil, "", mapS3Error(err)
	}
	defer out.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(out.Body, stateObjectLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > stateObjectLimit {
		return nil, "", errorsNew("state object exceeds 4 MiB")
	}
	return raw, aws.ToString(out.ETag), nil
}

func (s *s3Store) Create(ctx context.Context, key string, raw []byte) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(raw),
		ContentType: aws.String("application/json"), IfNoneMatch: aws.String("*")})
	return mapS3Error(err)
}

func (s *s3Store) Replace(ctx context.Context, key, version string, raw []byte) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(raw),
		ContentType: aws.String("application/json"), IfMatch: aws.String(version)})
	return mapS3Error(err)
}

// The tag lets a bucket lifecycle rule expire these noncurrent versions; a literal-prefix rule
// cannot, because the organization sits in the middle of the key.
func (s *s3Store) Put(ctx context.Context, key string, raw []byte) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(raw),
		ContentType: aws.String("application/json"), Tagging: aws.String("lifecycle=ephemeral")})
	return mapS3Error(err)
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	pager := awss3.NewListObjectsV2Paginator(s.client, &awss3.ListObjectsV2Input{Bucket: aws.String(s.bucket), Prefix: aws.String(prefix)})
	var out []ObjectInfo
	for pager.HasMorePages() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapS3Error(err)
		}
		for _, object := range page.Contents {
			out = append(out, ObjectInfo{Key: aws.ToString(object.Key), Version: aws.ToString(object.ETag)})
		}
	}
	return out, nil
}

func (s *s3Store) SourceHash(ctx context.Context, key string) (string, error) {
	head, err := s.client.HeadObject(ctx, &awss3.HeadObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key)})
	if err != nil {
		return "", mapS3Error(err)
	}
	// The SDK lowercases metadata names and strips the x-amz-meta- prefix.
	return head.Metadata["source-hash"], nil
}

func (s *s3Store) VersioningEnabled(ctx context.Context) (bool, error) {
	out, err := s.client.GetBucketVersioning(ctx, &awss3.GetBucketVersioningInput{Bucket: aws.String(s.bucket)})
	if err != nil {
		return false, mapS3Error(err)
	}
	return out.Status == types.BucketVersioningStatusEnabled, nil
}

func mapS3Error(err error) error {
	if err == nil {
		return nil
	}
	var api smithy.APIError
	if !errorsAs(err, &api) {
		return err
	}
	switch api.ErrorCode() {
	case "NoSuchKey", "NotFound":
		return ErrNotFound
	case "PreconditionFailed", "ConditionalRequestConflict":
		return ErrConflict
	default:
		return err
	}
}

func errorsAs(err error, target any) bool {
	switch value := target.(type) {
	case *smithy.APIError:
		for err != nil {
			if api, ok := err.(smithy.APIError); ok {
				*value = api
				return true
			}
			type unwrapper interface{ Unwrap() error }
			u, ok := err.(unwrapper)
			if !ok {
				break
			}
			err = u.Unwrap()
		}
	}
	return false
}

type s3Signer struct {
	bucket string
	client *awss3.Client
}

func (s *s3Signer) Authorize(ctx context.Context, _ InstallScope, batch UploadBatch) (TicketBatch, error) {
	lifetime := batch.ExpiresAt.Sub(batch.IssuedAt)
	if lifetime <= 0 {
		return TicketBatch{}, errorsNew("authorization already expired")
	}
	if rest := lifetime % time.Second; rest != 0 {
		lifetime += time.Second - rest
	}
	presigner := awss3.NewPresignClient(s.client)
	out := TicketBatch{Tickets: make([]uploadTicket, 0, len(batch.Objects))}
	for _, object := range batch.Objects {
		input := &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(object.Key), ContentLength: aws.Int64(object.Size), Metadata: object.Metadata}
		if object.Tagging != "" {
			input.Tagging = aws.String(object.Tagging)
		}
		signed, err := presigner.PresignPutObject(ctx, input, awss3.WithPresignExpires(lifetime))
		if err != nil {
			return TicketBatch{}, fmt.Errorf("presign object %s: %s", object.ObjectID, scrubURLs(err.Error()))
		}
		headers, lengthSigned, err := s3TicketHeaders(signed.SignedHeader)
		if err != nil {
			return TicketBatch{}, err
		}
		out.Tickets = append(out.Tickets, uploadTicket{TicketID: object.TicketID, ObjectID: object.ObjectID, Method: signed.Method, URL: signed.URL,
			ExpiresAt: batch.ExpiresAt, RequiredHeaders: headers, ContentLength: object.Size, ContentLengthSigned: lengthSigned})
	}
	return out, nil
}

func s3TicketHeaders(signed http.Header) (map[string]string, bool, error) {
	out := map[string]string{}
	lengthSigned := false
	for name, values := range signed {
		lower := strings.ToLower(name)
		if len(values) != 1 {
			return nil, false, fmt.Errorf("signed header %s has multiple values", lower)
		}
		switch {
		case lower == "host":
		case lower == "content-length":
			lengthSigned = true
		case lower == "x-amz-tagging" || strings.HasPrefix(lower, "x-amz-meta-"):
			out[lower] = values[0]
		default:
			return nil, false, fmt.Errorf("signed header %s is outside the ticket contract", lower)
		}
	}
	return out, lengthSigned, nil
}
