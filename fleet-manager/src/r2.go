package main

// Cloudflare R2 speaks the S3 API, so this reuses the S3 provider. R2 has no object tagging (a
// tagged PutObject is answered 501) and no object versioning, so a replaced object is gone.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

var r2AccountPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

type r2Store struct{ *s3Store }

type r2Signer struct{ *s3Signer }

func newR2(account, bucket, jurisdiction, accessKeyID, secretAccessKey string) (*r2Store, UploadSigner, error) {
	if !r2AccountPattern.MatchString(account) {
		return nil, nil, fmt.Errorf("--account must be a Cloudflare account ID (32 lowercase hex characters), not %q", account)
	}
	host := account
	switch jurisdiction {
	case "":
	case "eu", "fedramp":
		host += "." + jurisdiction
	default:
		return nil, nil, fmt.Errorf("FLEET_MANAGER_R2_JURISDICTION must be empty, eu or fedramp, not %q", jurisdiction)
	}
	if accessKeyID == "" || secretAccessKey == "" {
		return nil, nil, errors.New("FLEET_MANAGER_R2_ACCESS_KEY_ID and FLEET_MANAGER_R2_SECRET_ACCESS_KEY are required for R2")
	}
	// Path-style keeps the bucket in the ticket path, where the shipper's bucket probe reads it.
	client := awss3.New(awss3.Options{Region: "auto", BaseEndpoint: aws.String("https://" + host + ".r2.cloudflarestorage.com"), UsePathStyle: true,
		Credentials: credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, "")})
	return &r2Store{&s3Store{bucket: bucket, client: client}}, &r2Signer{&s3Signer{bucket: bucket, client: client}}, nil
}

func (s *r2Store) Put(ctx context.Context, key string, raw []byte) error {
	_, err := s.client.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(s.bucket), Key: aws.String(key), Body: bytes.NewReader(raw),
		ContentType: aws.String("application/json")})
	return mapS3Error(err)
}

func (s *r2Store) VersioningEnabled(context.Context) (bool, error) { return false, nil }

func (s *r2Signer) Authorize(ctx context.Context, scope InstallScope, batch UploadBatch) (TicketBatch, error) {
	for _, object := range batch.Objects {
		if object.Tagging != "" {
			return TicketBatch{}, fmt.Errorf("object %s carries tags, which R2 does not support", object.ObjectID)
		}
	}
	return s.s3Signer.Authorize(ctx, scope, batch)
}
