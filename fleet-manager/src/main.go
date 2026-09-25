package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

var version = "dev"

const help = `fleet-manager %s

Usage:
  fleet-manager --serve [flags]

Running without arguments prints this help without accessing cloud services.
Fleet administration is available in the web UI at /admin/.
`

type targetOptions struct{ provider, bucket, region, account string }

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.LUTC)
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		log.Printf("fleet-manager: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprintf(stdout, help, version)
		return err
	}
	if args[0] != "--serve" {
		return errors.New("usage: fleet-manager --serve [flags]")
	}
	return serveCommand(ctx, args[1:])
}

var serveCommand = runServe

func targetFlags(name string) (*flag.FlagSet, *targetOptions) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	target := &targetOptions{}
	fs.StringVar(&target.provider, "provider", os.Getenv("FLEET_MANAGER_PROVIDER"), "aws, gcp, or azure")
	fs.StringVar(&target.bucket, "bucket", os.Getenv("FLEET_MANAGER_BUCKET"), "bucket or Azure container")
	fs.StringVar(&target.region, "region", os.Getenv("FLEET_MANAGER_REGION"), "AWS region")
	fs.StringVar(&target.account, "account", os.Getenv("FLEET_MANAGER_ACCOUNT"), "GCP service account or Azure storage account")
	return fs, target
}

func openTarget(ctx context.Context, target *targetOptions) (*Manager, UploadSigner, error) {
	if target.bucket == "" {
		return nil, nil, errors.New("--bucket is required")
	}
	var store ObjectStore
	var signer UploadSigner
	var err error
	switch target.provider {
	case "aws":
		if target.region == "" {
			return nil, nil, errors.New("--region is required for AWS")
		}
		store, signer, err = newS3(ctx, target.bucket, target.region)
	case "gcp":
		if target.account == "" {
			return nil, nil, errors.New("--account is required for GCP signing")
		}
		store, signer, err = newGCS(ctx, target.bucket, target.account)
	case "azure":
		if target.account == "" {
			return nil, nil, errors.New("--account is required for Azure")
		}
		subscriptionID := os.Getenv("FLEET_MANAGER_AZURE_SUBSCRIPTION_ID")
		resourceGroup := os.Getenv("FLEET_MANAGER_AZURE_RESOURCE_GROUP")
		if subscriptionID == "" || resourceGroup == "" {
			return nil, nil, errors.New("FLEET_MANAGER_AZURE_SUBSCRIPTION_ID and FLEET_MANAGER_AZURE_RESOURCE_GROUP are required for Azure versioning verification")
		}
		store, signer, err = newAzure(target.account, target.bucket, subscriptionID, resourceGroup)
	default:
		return nil, nil, errors.New("--provider must be aws, gcp, or azure")
	}
	if err != nil {
		return nil, nil, err
	}
	if signer == nil {
		return nil, nil, errors.New("upload signer is unavailable")
	}
	manager, err := NewManager(store)
	return manager, signer, err
}

func runServe(ctx context.Context, args []string) error {
	fs, target := targetFlags("--serve")
	if err := fs.Parse(args); err != nil {
		return err
	}
	// Before the bucket is touched: a value nobody can have meant should stop the service at once.
	defaults, err := organizationDefaultsFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	manager, signer, err := openTarget(ctx, target)
	if err != nil {
		return err
	}
	server, err := NewServer(manager, signer, log.Default())
	if err != nil {
		return err
	}
	server.defaults = defaults
	identityContext, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	server.telemetry, err = telemetryForManager(identityContext, manager)
	if err != nil {
		return err
	}
	port := cmp.Or(os.Getenv("PORT"), os.Getenv("AWS_LWA_PORT"), "8080")
	if _, err := strconv.Atoi(port); err != nil {
		return errors.New("PORT must be numeric")
	}
	httpServer := &http.Server{Addr: ":" + port, Handler: server.Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Printf("serving multiple organizations with provider %s on port %s", target.provider, port)
	// Said at startup, because it decides what every organization that left a setting unset is
	// served, and nothing else in the log would show it.
	collector := cmp.Or(defaults.TelemetryCollectorURL, "none")
	log.Printf("organizations that state nothing get: Quesma ETL recipient %t, telemetry collector %s", defaults.AllowQuesmaETL, collector)
	return httpServer.ListenAndServe()
}
