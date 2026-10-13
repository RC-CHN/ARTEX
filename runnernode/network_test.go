package runnernode

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
)

func TestRemoteNetworkToolCAConfiguration(t *testing.T) {
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("verified private CA"))
	}))
	defer endpoint.Close()
	dir := t.TempDir()
	cert := filepath.Join(dir, "ca.pem")
	if e := os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw}), 0600); e != nil {
		t.Fatal(e)
	}
	fetchConfig, _ := json.Marshal(tool.WebFetchConfig{CACert: cert})
	spec := baseSpec(dir)
	spec.Env = map[string]string{"SSL_CERT_FILE": cert}
	spec.Tools = []runner.ToolDefinition{{Name: "WebFetch", Kind: "builtin", Exec: fetchConfig}, {Name: "private_http", Kind: "http", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}}, Exec: json.RawMessage(`{"url":"{endpoint}"}`)}}
	s, e := OpenLocalSession(context.Background(), t.Name(), spec)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	fetchInput, _ := json.Marshal(map[string]string{"url": endpoint.URL})
	r := s.Call(context.Background(), runner.ToolCall{ID: "fetch", Name: "WebFetch", Input: fetchInput})
	if r.Error != "" || r.Result.IsError || !strings.Contains(r.Result.Flatten(), "verified private CA") {
		t.Fatalf("WebFetch CA was not honored: %+v", r)
	}
	customInput, _ := json.Marshal(map[string]string{"endpoint": endpoint.URL})
	r = s.Call(context.Background(), runner.ToolCall{ID: "custom", Name: "private_http", Input: customInput})
	if r.Error != "" || r.Result.IsError || !strings.Contains(r.Result.Flatten(), "verified private CA") {
		t.Fatalf("custom HTTP session CA was not honored: %+v", r)
	}
}
func TestWebSearchManifestValidatedInSandbox(t *testing.T) {
	spec := baseSpec(t.TempDir())
	config, _ := json.Marshal(tool.WebSearchConfig{Backend: "brave-free"})
	spec.Tools = []runner.ToolDefinition{{Name: "web_search", Kind: "builtin", Exec: config}}
	if s, e := OpenLocalSession(context.Background(), t.Name(), spec); e == nil {
		s.Close()
		t.Fatal("missing search credential did not fail at open")
	}
	config, _ = json.Marshal(tool.WebSearchConfig{Backend: "brave-free", BraveAPIKey: "fixture-key", Proxy: "https://remote-proxy.invalid"})
	spec.Tools[0].Exec = config
	s, e := OpenLocalSession(context.Background(), t.Name()+"valid", spec)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if _, ok := s.registry.Get("web_search"); !ok {
		t.Fatal("web_search manifest not registered")
	}
	config, _ = json.Marshal(tool.WebSearchConfig{Backend: "brave-free", BraveAPIKey: "fixture-key", Proxy: "http://127.0.0.1:8080"})
	spec.Tools[0].Exec = config
	if s, e = OpenLocalSession(context.Background(), t.Name()+"loopback", spec); e == nil {
		s.Close()
		t.Fatal("controller loopback search proxy accepted")
	}
}
