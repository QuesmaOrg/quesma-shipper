//go:build live

package main

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
)

// Opt-in: FLEET_MANAGER_LIVE_PROVIDER plus the normal target variables drive a real store.
func TestLiveObjectStoreConformance(t *testing.T) {
	provider := os.Getenv("FLEET_MANAGER_LIVE_PROVIDER")
	if provider == "" {
		t.Skip("set FLEET_MANAGER_LIVE_PROVIDER")
	}
	org := "live-" + uuid.NewString()
	target := &targetOptions{provider: provider, bucket: os.Getenv("FLEET_MANAGER_BUCKET"),
		region: os.Getenv("FLEET_MANAGER_REGION"), account: os.Getenv("FLEET_MANAGER_ACCOUNT")}
	manager, _, err := openTarget(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	key := controlPrefix(org) + "conformance.json"
	t.Cleanup(func() { deleteLiveObject(t, ctx, manager.store, key) })
	if _, _, err := manager.store.Get(ctx, key); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absent read: %v", err)
	}
	if err := manager.store.Create(ctx, key, []byte(`{"schema":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.Create(ctx, key, []byte(`{"schema":1}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create: %v", err)
	}
	_, version, err := manager.store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.store.Replace(ctx, key, "stale", []byte(`{"schema":1}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale replace: %v", err)
	}
	if err := manager.store.Replace(ctx, key, version, []byte(`{"schema":1}`)); err != nil {
		t.Fatal(err)
	}
	objects, err := manager.store.List(ctx, controlPrefix(org))
	if err != nil || len(objects) != 1 {
		t.Fatalf("list: %#v %v", objects, err)
	}
	enabled, err := manager.store.VersioningEnabled(ctx)
	if err != nil || !enabled {
		t.Fatalf("versioning: %t %v", enabled, err)
	}
}

func deleteLiveObject(t *testing.T, ctx context.Context, store ObjectStore, key string) {
	t.Helper()
	switch value := store.(type) {
	case *s3Store:
		_, _ = value.client.DeleteObject(ctx, &awss3.DeleteObjectInput{Bucket: aws.String(value.bucket), Key: aws.String(key)})
	case *gcsStore:
		_ = value.bucket.Object(key).Delete(ctx)
	case *azureStore:
		_, _ = value.client.DeleteBlob(ctx, value.container, key, &azblob.DeleteBlobOptions{})
	default:
		t.Errorf("no cleanup for %T", store)
	}
}
