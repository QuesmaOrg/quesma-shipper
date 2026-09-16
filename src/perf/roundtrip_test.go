//go:build perf

package perf

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/QuesmaOrg/quesma-shipper/internal/transforms"
)

func TestUploadedTranscriptCanBeDownloadedAndDecrypted(t *testing.T) {
	w := stageWorld(t)
	session := corpusSessionID(0)
	body := fmt.Sprintf(corpusFirstLine, session, "/work/demo") +
		`{"type":"assistant","message":{"content":[{"type":"text","text":"download round trip ` + corpusGitHubToken + `"}]}}` + "\n"
	path := filepath.Join(w.Home, ".claude", "projects", "-work-demo", session+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	w.mustSync(t)

	var keys []string
	for _, key := range w.currentKeys(t) {
		if strings.HasPrefix(key, transcriptPrefix) {
			keys = append(keys, key)
		}
	}
	if len(keys) != 1 {
		t.Fatalf("uploaded %d transcripts, want 1", len(keys))
	}
	object, err := adminS3.GetObject(context.Background(), &awss3.GetObjectInput{
		Bucket: aws.String(w.bucket),
		Key:    aws.String(w.keyRoot + "/" + keys[0]),
	})
	if err != nil {
		t.Fatalf("download transcript: %v", err)
	}
	defer object.Body.Close()
	sealed, err := io.ReadAll(object.Body)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := age.ParseX25519Identity(testAgeIdentity)
	if err != nil {
		t.Fatal(err)
	}
	manifest, payload, err := transforms.Open(sealed, identity)
	if err != nil {
		t.Fatalf("decrypt and verify downloaded transcript: %v", err)
	}
	want := strings.ReplaceAll(body, corpusGitHubToken, "__REDACTED:github-pat__")
	if string(payload) != want {
		t.Fatalf("downloaded payload mismatch:\ngot:  %s\nwant: %s", payload, want)
	}
	if manifest.SourceHash != transforms.Hash([]byte(body)) || manifest.PayloadSize != int64(len(payload)) {
		t.Fatalf("downloaded manifest does not match the source and payload: %+v", manifest)
	}
	t.Logf("downloaded %d encrypted bytes; decrypted and verified %d payload bytes with the seeded token redacted", len(sealed), len(payload))
}
