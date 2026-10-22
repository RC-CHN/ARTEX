package recordexport

import (
	"encoding/json"
	"fmt"
	"io"
	"path"
)

// Static JS shards work on file:// without a server or network. Only 200 results
// are mounted at a time; a search scans shards serially with one shard in memory.
// The first page is also plain HTML so links/content remain available without JS.
type viewerRecord struct {
	HTML string `json:"html"`
}
type viewPages struct {
	b      *Builder
	w      io.Writer
	prefix string
	rows   []viewerRecord
	bytes  int
	parts  []string
}

func newViewPages(b *Builder, w io.Writer, prefix string) *viewPages {
	return &viewPages{b: b, w: w, prefix: prefix, parts: []string{}}
}
func (v *viewPages) add(markup string) error {
	if len(v.parts) == 0 {
		if _, err := io.WriteString(v.w, markup); err != nil {
			return err
		}
	}
	v.rows = append(v.rows, viewerRecord{HTML: markup})
	v.bytes += len(markup)
	if len(v.rows) >= 200 || v.bytes >= 2<<20 {
		return v.flush()
	}
	return nil
}
func (v *viewPages) flush() error {
	if len(v.rows) == 0 {
		return nil
	}
	name := fmt.Sprintf("%s.parts/%06d.js", v.prefix, len(v.parts)+1)
	data, err := json.Marshal(v.rows)
	if err != nil {
		return err
	}
	w, err := v.b.Open(name)
	if err != nil {
		return err
	}
	if _, err = io.WriteString(w, "window.artexRecords("); err == nil {
		_, err = w.Write(data)
	}
	if err == nil {
		_, err = io.WriteString(w, ");")
	}
	closeErr := w.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	v.parts = append(v.parts, "./"+path.Base(v.prefix)+".parts/"+path.Base(name))
	v.rows = nil
	v.bytes = 0
	return nil
}
func (v *viewPages) close() error {
	if err := v.flush(); err != nil {
		return err
	}
	parts, _ := json.Marshal(v.parts)
	_, err := fmt.Fprintf(v.w, `</div><nav><button id="prev" disabled>上一页</button> <button id="next">下一页</button> <span id="status" role="status"></span></nav><script>window.artexParts=%s;</script>%s`, parts, viewerScript)
	return err
}

const viewerScript = `<script>
(()=>{
const root=document.getElementById('records'),q=document.getElementById('q'),status=document.getElementById('status'),prev=document.getElementById('prev'),next=document.getElementById('next');
let generation=0,applied=-1,running=false,page=0,timer;
const load=src=>new Promise((resolve,reject)=>{let records;window.artexRecords=value=>{records=value};const s=document.createElement('script');s.src=src;s.onload=()=>{s.remove();delete window.artexRecords;Array.isArray(records)?resolve(records):reject(Error('invalid data'))};s.onerror=()=>{s.remove();delete window.artexRecords;reject(Error('unavailable data'))};document.head.append(s)});
async function refresh(){generation++;if(running)return;running=true;try{while(applied!==generation){const g=generation,query=q.value.toLowerCase(),skip=page*200;let matches=0,html=[],more=false;status.textContent='搜索中…';try{for(let i=0;i<window.artexParts.length;i++){const rows=await load(window.artexParts[i]);if(g!==generation)break;const template=document.createElement("template");for(const row of rows){if(query){template.innerHTML=row.html;if(!template.content.textContent.toLowerCase().includes(query))continue;}if(matches++<skip)continue;if(html.length===200){more=true;break}html.push(row.html)}status.textContent='已检查 '+(i+1)+' / '+window.artexParts.length+' 个分片';if(more)break}if(g!==generation)continue;root.innerHTML=html.join('');prev.disabled=page===0;next.disabled=!more;status.textContent='第 '+(page+1)+' 页 · 本页 '+html.length+' 条'+(more?' · 还有结果':' · 共 '+matches+' 条');}catch(e){if(g!==generation)continue;status.textContent='部分离线文件无法读取，请完整解压资料包后重试；原始 JSONL/Markdown 保留。'}applied=g;}}finally{running=false}}
q.addEventListener('input',()=>{page=0;generation++;clearTimeout(timer);timer=setTimeout(refresh,180)});prev.onclick=()=>{if(page>0){page--;refresh()}};next.onclick=()=>{page++;refresh()};refresh();
})();
</script></html>
`
