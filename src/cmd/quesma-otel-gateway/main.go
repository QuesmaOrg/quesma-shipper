// Command quesma-otel-gateway is the daemonless collection path: Claude Code and Codex export
// OpenTelemetry to it, and it ships what it receives through the shipper's own pipeline, one
// hosted install per person. It holds plaintext telemetry until the scrub, so run it where the
// organization's session data may be.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/QuesmaOrg/quesma-shipper/app"
	"github.com/QuesmaOrg/quesma-shipper/internal/gateway"
)

const usage = `usage: quesma-otel-gateway serve CONFIG_FILE
       quesma-otel-gateway token ORGANIZATION
       quesma-otel-gateway version`

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	switch {
	case len(args) == 2 && args[0] == "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
		if err := app.ServeGateway(ctx, args[1], app.NewBuild(), log); err != nil {
			fmt.Fprintln(os.Stderr, "quesma-otel-gateway:", err)
			return 1
		}
		return 0
	case len(args) == 2 && args[0] == "token":
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			fmt.Fprintln(os.Stderr, "quesma-otel-gateway:", err)
			return 1
		}
		token, sha := gateway.NewToken(args[1], hex.EncodeToString(secret))
		fmt.Printf("token (give to the agents, shown once): %s\ntoken_sha256 (put in the gateway config): %s\n", token, sha)
		return 0
	case len(args) == 1 && args[0] == "version":
		fmt.Println(app.VersionLine(app.NewBuild()))
		return 0
	}
	fmt.Fprintln(os.Stderr, usage)
	return 2
}
