package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	actool "github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
	"github.com/RC-CHN/ARTEX/runnerfiles"
)

var runnerInputMu sync.Mutex

// A content-addressed CA is immutable once published. Rewriting identical
// bytes would race ongoing downloads and invalidate their inode snapshots.
func publishRunnerCA(path string, data []byte) error {
	runnerInputMu.Lock()
	defer runnerInputMu.Unlock()
	current, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(current, data) {
			return fmt.Errorf("runner CA snapshot changed: %s", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ca-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Chmod(0644); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Server) runnerNetwork(spec *runner.RunSpec, plan *runnerfiles.Plan, env runnerEnvironment, cfg runnerConfig) error {
	proxy := env.ProxyURL
	if s.m.ProxyAddr() != "" && proxy == "" && !env.DirectNetwork {
		return fmt.Errorf("环境 %s 需要配置 runner 可达的 proxy_url；若明确不使用主服务录制/出口代理，请配置 direct_network=true", spec.Environment)
	}
	cert := ""
	if proxy != "" {
		for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY", "all_proxy"} {
			spec.Env[key] = proxy
		}
		spec.Env["NODE_USE_ENV_PROXY"] = "1"
		if source := s.m.ProxyCACert(); source != "" {
			b, e := os.ReadFile(source)
			if e != nil {
				return e
			}
			sum := sha256.Sum256(b)
			dir := filepath.Join(s.m.dir, "runner-inputs", "ca", hex.EncodeToString(sum[:]))
			if e = os.MkdirAll(dir, 0755); e != nil {
				return e
			}
			cert = filepath.Join(dir, "ca.pem")
			if e = publishRunnerCA(cert, b); e != nil {
				return e
			}
			plan.Mounts = append(plan.Mounts, runnerfiles.Mount{Name: "recording-ca", Source: dir, Target: dir})
			for _, key := range []string{"SSL_CERT_FILE", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO", "NODE_EXTRA_CA_CERTS"} {
				spec.Env[key] = cert
			}
		}
	}
	for i := range spec.MCPs {
		m := &spec.MCPs[i]
		if m.Name != browserMCPName || cfg.MCP[m.Name].Override != nil {
			continue
		}
		m.Args = stripProxyArgs(m.Args)
		delete(m.Env, "NODE_EXTRA_CA_CERTS")
		if proxy != "" {
			m.Args = append(m.Args, "--proxy-server", proxy)
			if cert != "" {
				if m.Env == nil {
					m.Env = map[string]string{}
				}
				m.Env["NODE_EXTRA_CA_CERTS"] = cert
			}
		}
	}
	for i := range spec.Tools {
		t := &spec.Tools[i]
		if t.Name == "WebFetch" {
			t.Exec, _ = json.Marshal(actool.WebFetchConfig{Proxy: proxy, CACert: cert})
		}
		if t.Name == "web_search" && t.Kind == "builtin" {
			var search actool.WebSearchConfig
			if err := json.Unmarshal(t.Exec, &search); err != nil {
				return err
			}
			search.Proxy, search.CACert = proxy, cert
			t.Exec, _ = json.Marshal(search)
		}
		if t.Kind == "http" {
			var conf httpExec
			if err := json.Unmarshal(t.Exec, &conf); err != nil {
				return err
			}
			if conf.UseRecordingProxy {
				if proxy == "" && !env.DirectNetwork {
					return fmt.Errorf("custom HTTP tool %s needs proxy_url", t.Name)
				}
				conf.Proxy = proxy
				conf.UseRecordingProxy = false
				t.Exec, _ = json.Marshal(struct {
					httpExec
					CACert string `json:"ca_cert,omitempty"`
				}{conf, cert})
			}
		}
	}
	plan.Mounts = append(plan.Mounts, env.Inputs...)
	return nil
}
