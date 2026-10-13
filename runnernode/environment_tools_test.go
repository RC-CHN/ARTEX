package runnernode

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/RC-CHN/ARTEX/runnerfiles"
)

func TestSandboxReportsItsOwnToolsOverRPC(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "custom-image"}[installed], func(t *testing.T) {
			bin := t.TempDir()
			if installed {
				// Discovery must not execute a binary simply to build a prompt.
				if err := os.WriteFile(filepath.Join(bin, "nuclei"), []byte("invalid executable, never run"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			spec := baseSpec(t.TempDir())
			spec.Env = map[string]string{"PATH": bin}
			p := &ProcessExecutor{Binary: os.Args[0], Env: []string{"ARTEX_RUNNER_TEST_HELPER=1"}}
			sandbox, err := p.Start(context.Background(), randomID(), spec, runnerfiles.Plan{}, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer sandbox.Stop()
			reply, err := sandbox.Request(context.Background(), SandboxRequest{Method: "open", SessionID: randomID(), Spec: &spec})
			if err != nil || reply.Error != "" {
				t.Fatalf("open: %+v %v", reply, err)
			}
			if (reply.EnvironmentTools.Nuclei != nil) != installed || reply.EnvironmentTools.Vulnx != nil {
				t.Fatalf("inventory did not reflect this sandbox: %+v", reply.EnvironmentTools)
			}
			if installed && reply.EnvironmentTools.Nuclei.Version != "" {
				t.Fatal("custom image falsely reported bundled tool version")
			}
		})
	}
}
