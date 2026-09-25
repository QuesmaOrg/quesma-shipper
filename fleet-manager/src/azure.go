package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/storage/armstorage"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/sas"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/service"
)

type azureStore struct {
	container  string
	client     *azblob.Client
	versioning func(context.Context) (bool, error)
}
type delegationSource interface {
	GetUserDelegationCredential(context.Context, service.KeyInfo, *service.GetUserDelegationCredentialOptions) (*service.UserDelegationCredential, error)
}
type azureSigner struct {
	account, container string
	delegation         delegationSource
	signSAS            func(sas.BlobSignatureValues, *service.UserDelegationCredential) (string, error)
	clock              func() time.Time
	mu                 sync.Mutex
	cached             *service.UserDelegationCredential
	keyExpires         time.Time
}

func newAzure(account, containerName, subscriptionID, resourceGroup string) (*azureStore, UploadSigner, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, nil, fmt.Errorf("load Azure credentials: %w", err)
	}
	endpoint := "https://" + account + ".blob.core.windows.net/"
	client, err := azblob.NewClient(endpoint, credential, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create Azure Blob client: %w", err)
	}
	serviceClient, err := service.NewClient(endpoint, credential, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create Azure delegation client: %w", err)
	}
	armClient, err := armstorage.NewBlobServicesClient(subscriptionID, credential, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("create Azure Resource Manager client: %w", err)
	}
	store := &azureStore{container: containerName, client: client}
	store.versioning = func(ctx context.Context) (bool, error) {
		response, err := armClient.GetServiceProperties(ctx, resourceGroup, account, nil)
		if err != nil {
			return false, err
		}
		properties := response.BlobServiceProperties.BlobServiceProperties
		return properties != nil && properties.IsVersioningEnabled != nil && *properties.IsVersioningEnabled, nil
	}
	return store,
		&azureSigner{account: account, container: containerName, delegation: serviceClient, clock: time.Now,
			signSAS: func(values sas.BlobSignatureValues, credential *service.UserDelegationCredential) (string, error) {
				query, err := values.SignWithUserDelegation(credential)
				if err != nil {
					return "", err
				}
				return query.Encode(), nil
			}}, nil
}

func (s *azureStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	response, err := s.client.DownloadStream(ctx, s.container, key, nil)
	if err != nil {
		return nil, "", mapAzureError(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, stateObjectLimit+1))
	if err != nil {
		return nil, "", err
	}
	if len(raw) > stateObjectLimit {
		return nil, "", errorsNew("state object exceeds 4 MiB")
	}
	if response.ETag == nil {
		return nil, "", errorsNew("Azure response has no ETag")
	}
	return raw, string(*response.ETag), nil
}

func (s *azureStore) write(ctx context.Context, key string, raw []byte, conditions *blob.ModifiedAccessConditions) error {
	_, err := s.client.UploadBuffer(ctx, s.container, key, raw, &azblob.UploadBufferOptions{AccessConditions: &blob.AccessConditions{ModifiedAccessConditions: conditions}})
	return mapAzureError(err)
}

func (s *azureStore) Create(ctx context.Context, key string, raw []byte) error {
	any := azcore.ETagAny
	return s.write(ctx, key, raw, &blob.ModifiedAccessConditions{IfNoneMatch: &any})
}

func (s *azureStore) Replace(ctx context.Context, key, version string, raw []byte) error {
	etag := azcore.ETag(version)
	return s.write(ctx, key, raw, &blob.ModifiedAccessConditions{IfMatch: &etag})
}

func (s *azureStore) Put(ctx context.Context, key string, raw []byte) error {
	return s.write(ctx, key, raw, nil)
}

func (s *azureStore) List(ctx context.Context, prefix string) ([]ObjectInfo, error) {
	pager := s.client.NewListBlobsFlatPager(s.container, &container.ListBlobsFlatOptions{Prefix: &prefix})
	var out []ObjectInfo
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, mapAzureError(err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item.Name == nil || item.Properties == nil || item.Properties.ETag == nil {
				continue
			}
			out = append(out, ObjectInfo{Key: *item.Name, Version: string(*item.Properties.ETag)})
		}
	}
	return out, nil
}

func (s *azureStore) SourceHash(ctx context.Context, key string) (string, error) {
	props, err := s.client.ServiceClient().NewContainerClient(s.container).NewBlobClient(key).GetProperties(ctx, nil)
	if err != nil {
		return "", mapAzureError(err)
	}
	// The upload SAS wrote x-ms-meta-source_hash; the service echoes metadata names in
	// whatever casing the transport canonicalized them to.
	for name, value := range props.Metadata {
		if strings.EqualFold(name, "source_hash") && value != nil {
			return *value, nil
		}
	}
	return "", nil
}

// The Blob data API does not expose account-level versioning configuration. Azure startup
// therefore requires the deployment to pass an explicit assertion produced by its ARM check.
func (s *azureStore) VersioningEnabled(ctx context.Context) (bool, error) {
	return s.versioning(ctx)
}

func mapAzureError(err error) error {
	if err == nil {
		return nil
	}
	if response, ok := err.(*azcore.ResponseError); ok {
		if response.StatusCode == http.StatusNotFound {
			return ErrNotFound
		}
		if response.StatusCode == http.StatusPreconditionFailed || response.StatusCode == http.StatusConflict {
			return ErrConflict
		}
	}
	return err
}

func (s *azureSigner) Authorize(ctx context.Context, _ InstallScope, batch UploadBatch) (TicketBatch, error) {
	now := time.Now().UTC()
	if s.clock != nil {
		now = s.clock().UTC()
	}
	credential, err := s.delegationCredential(ctx, now)
	if err != nil {
		return TicketBatch{}, err
	}
	out := TicketBatch{Tickets: make([]uploadTicket, 0, len(batch.Objects))}
	for _, object := range batch.Objects {
		headers := make(map[string]string, len(object.Metadata)+2)
		for name, value := range object.Metadata {
			headers["x-ms-meta-"+strings.ReplaceAll(strings.ToLower(name), "-", "_")] = value
		}
		headers["x-ms-blob-type"] = "BlockBlob"
		if object.Tagging != "" {
			headers["x-ms-tags"] = object.Tagging
		}
		signedHeaders := make(map[string]string, len(headers)+1)
		for name, value := range headers {
			signedHeaders[name] = value
		}
		signedHeaders["content-length"] = strconv.FormatInt(object.Size, 10)
		permissions := (&sas.BlobPermissions{Create: true, Write: true, Tag: object.Tagging != ""}).String()
		query, err := s.signSAS(sas.BlobSignatureValues{Protocol: sas.ProtocolHTTPS, StartTime: now.Add(-time.Minute), ExpiryTime: batch.ExpiresAt,
			Permissions: permissions, ContainerName: s.container, BlobName: object.Key, SignedRequestHeaders: signedHeaders}, credential)
		if err != nil {
			return TicketBatch{}, fmt.Errorf("presign object %s: %s", object.ObjectID, scrubURLs(err.Error()))
		}
		u := &url.URL{Scheme: "https", Host: s.account + ".blob.core.windows.net", Path: "/" + s.container + "/" + object.Key, RawQuery: query}
		out.Tickets = append(out.Tickets, uploadTicket{TicketID: object.TicketID, ObjectID: object.ObjectID, Method: http.MethodPut,
			URL: u.String(), ExpiresAt: batch.ExpiresAt, RequiredHeaders: headers, ContentLength: object.Size, ContentLengthSigned: true})
	}
	return out, nil
}

func (s *azureSigner) delegationCredential(ctx context.Context, now time.Time) (*service.UserDelegationCredential, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && now.Before(s.keyExpires.Add(-5*time.Minute)) {
		return s.cached, nil
	}
	start, expiry := now.Add(-5*time.Minute), now.Add(6*time.Hour)
	credential, err := s.delegation.GetUserDelegationCredential(ctx, service.KeyInfo{Start: to.Ptr(start.Format(time.RFC3339)), Expiry: to.Ptr(expiry.Format(time.RFC3339))}, nil)
	if err != nil {
		return nil, fmt.Errorf("obtain Azure user-delegation key: %s", scrubURLs(err.Error()))
	}
	s.cached, s.keyExpires = credential, expiry
	return credential, nil
}
