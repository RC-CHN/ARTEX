package runnernode

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/RC-CHN/ARTEX/norma/tool"
	"github.com/RC-CHN/ARTEX/runner"
)

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
func renderTemplate(t string, params map[string]any, quote func(string) string) string {
	// Preserve existing {parameter} syntax; replacement values are not re-scanned.
	var b strings.Builder
	for len(t) > 0 {
		a := strings.IndexByte(t, '{')
		if a < 0 {
			b.WriteString(t)
			break
		}
		b.WriteString(t[:a])
		t = t[a:]
		z := strings.IndexByte(t, '}')
		if z < 0 {
			b.WriteString(t)
			break
		}
		key := t[1:z]
		if v, ok := params[key]; ok {
			switch vv := v.(type) {
			case string:
				b.WriteString(quote(vv))
			default:
				j, _ := json.Marshal(v)
				b.WriteString(quote(string(j)))
			}
		} else {
			b.WriteString(t[:z+1])
		}
		t = t[z+1:]
	}
	return b.String()
}
func customTool(def runner.ToolDefinition) (tool.CoreTool, error) {
	var spec struct {
		Command           string            `json:"command"`
		Code              string            `json:"code"`
		Interpreter       string            `json:"interpreter"`
		TimeoutMS         int               `json:"timeout_ms"`
		Method            string            `json:"method"`
		URL               string            `json:"url"`
		Headers           map[string]string `json:"headers"`
		Body              string            `json:"body"`
		Proxy             string            `json:"proxy"`
		UseRecordingProxy bool              `json:"use_recording_proxy"`
		CACert            string            `json:"ca_cert"`
		InsecureTLS       bool              `json:"insecure_tls"`
	}
	if err := json.Unmarshal(def.Exec, &spec); err != nil {
		return nil, fmt.Errorf("tool %s manifest: %w", def.Name, err)
	}
	if def.Kind == "http" && (spec.UseRecordingProxy || controllerLocalURL(spec.Proxy) || controllerLocalURL(spec.URL)) {
		return nil, fmt.Errorf("tool %s requires a remote-reachable HTTP/proxy manifest", def.Name)
	}
	return tool.Build(tool.Spec{Name: def.Name, Description: def.Description, Schema: def.InputSchema, Run: func(ctx context.Context, input json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
		var params map[string]any
		if err := json.Unmarshal(input, &params); err != nil {
			return tool.Result{}, err
		}
		timeout := 120 * time.Second
		if spec.TimeoutMS > 0 {
			timeout = time.Duration(spec.TimeoutMS) * time.Millisecond
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		switch def.Kind {
		case "command":
			if strings.TrimSpace(spec.Command) == "" {
				return tool.Errorf("command is empty"), nil
			}
			raw, _ := json.Marshal(map[string]any{"command": renderTemplate(spec.Command, params, shellQuote), "timeout_ms": int(timeout / time.Millisecond)})
			return tool.NewBash().Call(ctx, raw, tc)
		case "script":
			if strings.TrimSpace(spec.Code) == "" {
				return tool.Errorf("Python script is empty"), nil
			}
			dir := filepath.Join(tc.WorkingDir, ".tools")
			if e := os.MkdirAll(dir, 0700); e != nil {
				return tool.Result{}, e
			}
			f, e := os.CreateTemp(dir, "runner-*.py")
			if e != nil {
				return tool.Result{}, e
			}
			file := f.Name()
			defer os.Remove(file)
			if _, e = f.WriteString(spec.Code); e != nil {
				f.Close()
				return tool.Result{}, e
			}
			if e = f.Close(); e != nil {
				return tool.Result{}, e
			}
			interp := spec.Interpreter
			if interp == "" {
				interp = "python3"
			}
			localTC := *tc
			localTC.Env = append([]string{}, tc.Env...)
			for k, v := range params {
				if envKey.MatchString(k) {
					switch v.(type) {
					case string, float64, bool:
						localTC.Env = append(localTC.Env, "TOOL_"+strings.ToUpper(k)+"="+fmt.Sprint(v))
					}
				}
			}
			command := "printf '%s' " + shellQuote(string(input)) + " | " + shellQuote(interp) + " " + shellQuote(file)
			raw, _ := json.Marshal(map[string]any{"command": command, "timeout_ms": int(timeout / time.Millisecond)})
			return tool.NewBash().Call(ctx, raw, &localTC)
		case "http":
			identity := func(v string) string { return v }
			method := strings.ToUpper(spec.Method)
			if method == "" {
				method = "GET"
			}
			req, e := http.NewRequestWithContext(ctx, method, renderTemplate(spec.URL, params, identity), strings.NewReader(renderTemplate(spec.Body, params, identity)))
			if e != nil {
				return tool.Result{}, e
			}
			for k, v := range spec.Headers {
				req.Header.Set(k, renderTemplate(v, params, identity))
			}
			tr := http.DefaultTransport.(*http.Transport).Clone()
			defer tr.CloseIdleConnections()
			cert := spec.CACert
			if cert == "" && tc != nil {
				for _, pair := range tc.Env {
					if k, v, ok := strings.Cut(pair, "="); ok && k == "SSL_CERT_FILE" {
						cert = v
					}
				}
			}
			if spec.InsecureTLS {
				tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			} else if cert != "" {
				pem, e := os.ReadFile(cert)
				if e != nil {
					return tool.Result{}, fmt.Errorf("custom HTTP CA file: %w", e)
				}
				pool, _ := x509.SystemCertPool()
				if pool == nil {
					pool = x509.NewCertPool()
				}
				if !pool.AppendCertsFromPEM(pem) {
					return tool.Result{}, fmt.Errorf("custom HTTP CA file contains no valid certificate")
				}
				tr.TLSClientConfig = &tls.Config{RootCAs: pool}
			}
			if spec.Proxy != "" {
				pu, e := url.Parse(spec.Proxy)
				if e != nil {
					return tool.Result{}, e
				}
				tr.Proxy = http.ProxyURL(pu)
			}
			response, e := (&http.Client{Transport: tr}).Do(req)
			if e != nil {
				return tool.Result{}, e
			}
			defer response.Body.Close()
			body, e := io.ReadAll(io.LimitReader(response.Body, 1<<20))
			if e != nil {
				return tool.Result{}, e
			}
			encoded, _ := json.Marshal(map[string]any{"status": response.StatusCode, "body": string(body)})
			return tool.Text(tool.Capture(tc, string(encoded))), nil
		}
		return tool.Result{}, fmt.Errorf("unsupported custom tool kind")
	}}), nil
}
