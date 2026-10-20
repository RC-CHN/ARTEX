package recordexport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/url"
	"os"
	"strings"
	"time"
)

const pageStart = `<!doctype html><html lang="zh-CN"><meta charset="utf-8"><meta name="viewport" content="width=device-width"><meta http-equiv="Content-Security-Policy" content="default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'"><title>ARTEX 任务记录</title><style>body{font:16px system-ui;max-width:1100px;margin:32px auto;padding:0 20px;color:#16202b;background:#f7f9fc}input,select{padding:10px;font:inherit;max-width:90%}article,details{background:white;border:1px solid #d9e0e8;padding:14px;margin:8px 0;border-radius:8px}pre{white-space:pre-wrap;overflow-wrap:anywhere;font:13px ui-monospace,monospace}a{color:#075aab}small{color:#506070}.omission{border-color:#c58022}[hidden]{display:none!important}</style>`
const pageEnd = `<script>const q=document.getElementById('q');if(q)q.addEventListener('input',()=>{const s=q.value.toLowerCase();for(const e of document.querySelectorAll('[data-record]'))e.hidden=!e.textContent.toLowerCase().includes(s)});</script></html>`

func linkPath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return "./" + strings.Join(parts, "/")
}
func (b *Builder) Index() error {
	w, err := b.Open("index.html")
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(w, pageStart, `<h1>ARTEX 任务记录资料包</h1><p>离线查看；仅包含所选任务。数据库快照：`, html.EscapeString(b.Manifest.Cutoff.Format(time.RFC3339)), `</p><p><a href="./manifest.json" download>完整清单、SHA-256 与缺失说明</a></p><input id="q" aria-label="筛选文件" placeholder="按任务、会话、文件名或缺失原因筛选">`)
	if err == nil && b.Manifest.Redacted {
		_, err = io.WriteString(w, `<p>这是凭据字段过滤副本。二进制和过大的文件会列为缺失；自动过滤不保证已覆盖所有敏感内容，外发前仍需审阅。</p>`)
	}
	for _, f := range b.Manifest.Files {
		if err != nil {
			break
		}
		download := " download"
		parts := strings.Split(f.Path, "/")
		if len(parts) == 4 && parts[0] == "tasks" && parts[2] == "transcripts" && strings.HasSuffix(parts[3], ".html") {
			download = ""
		}
		_, err = fmt.Fprintf(w, `<article data-record><a href="%s"%s>%s</a><br><small>%d bytes · SHA-256 %s</small></article>`, html.EscapeString(linkPath(f.Path)), download, html.EscapeString(f.Path), f.Size, f.SHA256)
	}
	for _, m := range b.Manifest.Omissions {
		if err != nil {
			break
		}
		_, err = fmt.Fprintf(w, `<article class="omission" data-record><strong>未包含</strong> %s<br><small>%s</small></article>`, html.EscapeString(m.Path), html.EscapeString(m.Reason))
	}
	if err == nil {
		_, err = io.WriteString(w, pageEnd)
	}
	return errors.Join(err, w.Close())
}

// Transcript copies only complete records at/before the database snapshot. It
// reads at most the initial file length, so concurrent appends cannot prolong it.
func (b *Builder) Transcript(source, prefix string) error {
	before, err := os.Lstat(source)
	if os.IsNotExist(err) {
		b.Omit(prefix, "transcript_not_present")
		return nil
	}
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		b.Omit(prefix, "transcript_not_regular")
		return nil
	}
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		return fmt.Errorf("transcript changed before open")
	}
	raw, err := b.Open(prefix + ".jsonl")
	if err != nil {
		return err
	}
	if b.Manifest.Redacted {
		raw = RedactJSON(raw)
	}
	md, err := b.Open(prefix + ".md")
	if err != nil {
		raw.Close()
		return err
	}
	view, err := b.Open(prefix + ".html")
	if err != nil {
		raw.Close()
		md.Close()
		return err
	}
	run := func() error {
		if _, err := fmt.Fprintln(md, "# 原始会话记录\n\n每条记录保留 UUID、角色、工具参数与返回内容。读取时点：", b.Manifest.Cutoff.Format(time.RFC3339)); err != nil {
			return err
		}
		if _, err := io.WriteString(view, pageStart+`<h1>会话记录</h1><input id="q" aria-label="搜索记录" placeholder="搜索角色、工具、原文或 UUID">`); err != nil {
			return err
		}
		dec := json.NewDecoder(&contextReader{ctx: b.ctx, r: io.LimitReader(f, before.Size())})
		number := 0
		for {
			if err := b.ctx.Err(); err != nil {
				return err
			}
			if b.omissionOverflow {
				return ErrCapacity
			}
			var row json.RawMessage
			err := dec.Decode(&row)
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Omit(prefix, "invalid_or_partial_transcript_tail")
				break
			}
			var meta struct {
				UUID, Timestamp, Type string
				Message               struct{ Role string } `json:"message"`
			}
			if err = json.Unmarshal(row, &meta); err != nil {
				return err
			}
			stamp, e := time.Parse(time.RFC3339Nano, meta.Timestamp)
			if e != nil {
				b.Omit(prefix, "record_without_valid_timestamp")
				continue
			}
			if stamp.After(b.Manifest.Cutoff) {
				continue
			}
			if _, err = raw.Write(append(append([]byte(nil), row...), '\n')); err != nil {
				return err
			}
			number++
			display := row
			if b.Manifest.Redacted {
				var v any
				decoder := json.NewDecoder(bytes.NewReader(row))
				decoder.UseNumber()
				if err = decoder.Decode(&v); err != nil {
					return err
				}
				display, err = json.MarshalIndent(scrub(v), "", "  ")
				if err != nil {
					return err
				}
			} else {
				var out bytes.Buffer
				if err = json.Indent(&out, row, "", "  "); err == nil {
					display = out.Bytes()
				}
			}
			label := fmt.Sprintf("%d · %s · %s · %s", number, meta.Timestamp, meta.Message.Role, meta.UUID)
			if _, err = fmt.Fprintf(md, "\n## %s\n\n````json\n%s\n````\n", label, display); err != nil {
				return err
			}
			if _, err = fmt.Fprintf(view, `<details data-record id="record-%d"><summary>%s</summary><pre>%s</pre></details>`, number, html.EscapeString(label), html.EscapeString(string(display))); err != nil {
				return err
			}
		}
		after, e := f.Stat()
		if e != nil || after.Size() < before.Size() || after.ModTime() != before.ModTime() && after.Size() == before.Size() {
			b.Omit(prefix, "transcript_changed_during_copy_review_raw_records")
		}
		_, err := io.WriteString(view, pageEnd)
		return err
	}
	err = run()
	return errors.Join(err, raw.Close(), md.Close(), view.Close())
}
