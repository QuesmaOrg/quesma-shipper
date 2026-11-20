package main

// Cloudflare R2 speaks the S3 API, so this configures the S3 provider for it. R2 has no object
// tagging (a tagged PutObject is answered 501) and no object versioning, so a replaced object is gone.

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

var r2AccountPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func newR2(account, bucket, jurisdiction, accessKeyID, secretAccessKey string) (*s3Store, UploadSigner, error) {
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
	return &s3Store{bucket: bucket, client: client, untagged: true, unversioned: true}, &s3Signer{bucket: bucket, client: client, untagged: true}, nil
}
