// artex-runner is a standalone outbound-only execution node. The --sandbox mode
// is private to the attached container protocol and never listens on a socket.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/RC-CHN/ARTEX/runnernode"
)

func main() {
	if err := run(); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "artex-runner:", err)
		os.Exit(1)
	}
}
func run() error {
	config := flag.String("config", "runner.json", "runner configuration file")
	tokenFile := flag.String("token-file", "", "one-time enrollment token file (not persisted)")
	sandbox := flag.Bool("sandbox", false, "private sandbox JSON-lines mode")
	heartbeat := flag.Duration("heartbeat-timeout", 0, "sandbox exits if its manager stops sending requests (native Pods)")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *sandbox {
		if *heartbeat < 0 || *heartbeat > 5*time.Minute {
			return fmt.Errorf("invalid heartbeat timeout")
		}
		return runnernode.ServeSandboxWithHeartbeat(ctx, os.Stdin, os.Stdout, *heartbeat)
	}
	c, err := runnernode.LoadConfig(*config)
	if err != nil {
		return err
	}
	token := os.Getenv("ARTEX_RUNNER_ENROLLMENT_TOKEN")
	_ = os.Unsetenv("ARTEX_RUNNER_ENROLLMENT_TOKEN")
	if *tokenFile != "" {
		// After enrollment, deployments can remove the one-use Secret without
		// changing their command or losing the persisted node identity.
		if _, e := os.Stat(filepath.Join(c.DataDir, "credentials.json")); errors.Is(e, os.ErrNotExist) {
			b, e := os.ReadFile(*tokenFile)
			if e != nil {
				return e
			}
			token = strings.TrimSpace(string(b))
		}
	}
	node, err := runnernode.New(c)
	if err != nil {
		return err
	}
	return node.Run(ctx, token)
}
