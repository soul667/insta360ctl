package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urfave/cli/v2"
	"github.com/xaionaro-go/insta360ctl/pkg/multi"
)

// cmdMultiServe runs a small local web console for the multi-camera manager.
// The BLE connections are established once at startup and kept open; every
// button on the page triggers a synchronized broadcast to all cameras.
func cmdMultiServe() *cli.Command {
	return &cli.Command{
		Name:  "serve",
		Usage: "Serve a local web console: click Record/Stop to control all cameras",
		Flags: append(commonMultiFlags(),
			&cli.StringFlag{
				Name:  "listen",
				Value: "127.0.0.1:8787",
				Usage: "HTTP listen address (use 0.0.0.0:8787 to allow phones on the LAN)",
			},
		),
		Action: multiServe,
	}
}

type webResultRow struct {
	Name       string  `json:"name"`
	Addr       string  `json:"addr"`
	DispatchMS float64 `json:"dispatch_ms"`
	RTTMS      float64 `json:"rtt_ms"`
	OK         bool    `json:"ok"`
	Error      string  `json:"error,omitempty"`
	File       string  `json:"file,omitempty"`
}

type webReport struct {
	Command    string        `json:"command"`
	At         string        `json:"at"`
	Rows       []webResultRow `json:"rows"`
	SpreadMS   float64       `json:"spread_ms"`
	Failed     int           `json:"failed"`
	ClockDiffS *float64      `json:"clock_diff_s,omitempty"`
	ClockNote  string        `json:"clock_note,omitempty"`
}

type webCamera struct {
	Name  string `json:"name"`
	Addr  string `json:"addr"`
	Model string `json:"model"`
}

type webStatus struct {
	Recording bool        `json:"recording"`
	Busy      bool        `json:"busy"`
	Cameras   []webCamera `json:"cameras"`
	Last      *webReport  `json:"last,omitempty"`
}

type webState struct {
	mu        sync.Mutex
	mgr       *multi.Manager
	recording bool
	busy      bool
	last      *webReport
}

var fileClockRe = regexp.MustCompile(`VID_(\d{8})_(\d{6})`)

func parseFileClock(file string) (time.Time, bool) {
	m := fileClockRe.FindStringSubmatch(file)
	if m == nil {
		return time.Time{}, false
	}
	t, err := time.Parse("20060102_150405", m[1]+"_"+m[2])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// run broadcasts one action and returns the report, or nil when busy.
func (s *webState) run(ctx context.Context, name string, action multi.Action, delay time.Duration) *webReport {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return nil
	}
	s.busy = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.busy = false
		s.mu.Unlock()
	}()

	var fireAt time.Time
	if delay > 0 {
		fireAt = time.Now().Add(delay)
	}
	results := s.mgr.BroadcastAt(ctx, fireAt, action)

	rep := &webReport{Command: name, At: time.Now().Format("15:04:05")}
	base := time.Time{}
	for _, r := range results {
		if r.DispatchedAt.IsZero() {
			continue
		}
		if base.IsZero() || r.DispatchedAt.Before(base) {
			base = r.DispatchedAt
		}
	}
	type fileStamp struct {
		name string
		t    time.Time
	}
	var stamps []fileStamp
	for _, r := range results {
		row := webResultRow{Name: r.Device.Name(), Addr: r.Device.Address, OK: r.Err == nil}
		if !r.DispatchedAt.IsZero() {
			row.DispatchMS = float64(r.DispatchedAt.Sub(base)) / float64(time.Millisecond)
		}
		if !r.DispatchedAt.IsZero() && !r.FinishedAt.IsZero() {
			row.RTTMS = float64(r.RTT()) / float64(time.Millisecond)
		}
		if r.Err != nil {
			row.Error = r.Err.Error()
			rep.Failed++
		}
		for _, line := range strings.Split(string(r.Output), "\n") {
			if strings.HasPrefix(line, "file: ") {
				row.File = strings.TrimPrefix(line, "file: ")
			}
		}
		if row.File != "" {
			if t, ok := parseFileClock(row.File); ok {
				stamps = append(stamps, fileStamp{name: r.Device.Name(), t: t})
			}
		}
		rep.Rows = append(rep.Rows, row)
	}
	sm := multi.Summarize(results)
	rep.SpreadMS = float64(sm.DispatchSpread) / float64(time.Millisecond)

	if name == "record stop" && len(stamps) >= 2 {
		d := stamps[0].t.Sub(stamps[1].t).Seconds()
		rep.ClockDiffS = &d
		rep.ClockNote = fmt.Sprintf("%s 的时钟比 %s 快 %+.0f 秒（按录制起点）", stamps[0].name, stamps[1].name, d)
	}

	s.mu.Lock()
	s.last = rep
	if name == "record start" && rep.Failed == 0 {
		s.recording = true
	}
	if name == "record stop" {
		s.recording = false
	}
	s.mu.Unlock()
	return rep
}

func (s *webState) writeStatus(w http.ResponseWriter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := webStatus{Recording: s.recording, Busy: s.busy, Last: s.last}
	for _, d := range s.mgr.Devices {
		st.Cameras = append(st.Cameras, webCamera{Name: d.Name(), Addr: d.Address, Model: d.Model().String()})
	}
	writeJSON(w, http.StatusOK, st)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func multiServe(c *cli.Context) error {
	ctx, mgr, cleanup, err := connectMultiManager(c)
	if err != nil {
		return err
	}
	defer cleanup()

	st := &webState{mgr: mgr}

	actionHandler := func(name string, makeAction func() multi.Action) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "POST required"})
				return
			}
			var delay time.Duration
			if ms := r.URL.Query().Get("delay_ms"); ms != "" {
				if n, err := strconv.Atoi(ms); err == nil && n > 0 {
					delay = time.Duration(n) * time.Millisecond
				}
			}
			rep := st.run(ctx, name, makeAction(), delay)
			if rep == nil {
				writeJSON(w, http.StatusConflict, map[string]string{"error": "busy, another command is still running"})
				return
			}
			writeJSON(w, http.StatusOK, rep)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(webConsoleHTML))
	})
	mux.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) { st.writeStatus(w) })
	mux.HandleFunc("/api/start", actionHandler("record start", func() multi.Action { return recordAction(true, false) }))
	mux.HandleFunc("/api/stop", actionHandler("record stop", func() multi.Action { return recordAction(false, false) }))
	mux.HandleFunc("/api/photo", actionHandler("photo", func() multi.Action {
		_, action, _ := resolveMultiAction([]string{"photo"}, false)
		return action
	}))
	mux.HandleFunc("/api/marker", actionHandler("marker", func() multi.Action {
		_, action, _ := resolveMultiAction([]string{"marker"}, false)
		return action
	}))

	listen := c.String("listen")
	fmt.Printf("Dual-camera web console: http://%s\n", listen)
	fmt.Println("Press Ctrl+C to stop.")
	if err := http.ListenAndServe(listen, mux); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

const webConsoleHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>X5 双机控制台</title>
<style>
  body{font-family:system-ui,-apple-system,"Segoe UI",Roboto,"Noto Sans SC",sans-serif;background:#111;color:#eee;max-width:780px;margin:0 auto;padding:24px}
  h1{font-size:20px;font-weight:600;margin:0 0 14px}
  .row{display:flex;gap:14px;margin:16px 0;align-items:center}
  button{flex:1;padding:22px 10px;font-size:22px;font-weight:700;border:none;border-radius:14px;cursor:pointer;color:#fff}
  button:disabled{opacity:.4;cursor:not-allowed}
  #start{background:#18a058}#stop{background:#d03050}
  .small button{font-size:16px;padding:12px;background:#333;flex:0 0 auto;min-width:110px}
  #banner{padding:10px 14px;border-radius:10px;background:#1d1d1d;margin:10px 0;font-size:14px;line-height:1.7}
  table{width:100%;border-collapse:collapse;font-size:14px;margin-top:10px}
  th,td{padding:8px 6px;border-bottom:1px solid #2a2a2a;text-align:left;vertical-align:top}
  .ok{color:#63e2b7}.err{color:#e88080}
  .cam{font-size:12px;color:#999;word-break:break-all}
  .clock{color:#ffd666;font-weight:700}
  input[type=number]{width:64px;background:#222;color:#eee;border:1px solid #444;border-radius:8px;padding:8px}
</style>
</head>
<body>
<h1>X5 双机控制台</h1>
<div id="banner">连接中…</div>
<div class="row">
  <button id="start" onclick="doStart()">● 开始录制</button>
  <button id="stop" onclick="doStop()">■ 停止录制</button>
</div>
<div class="row small">
  <button onclick="act('/api/photo','拍照')">📷 拍照</button>
  <button onclick="act('/api/marker','标记')">🚩 高亮标记</button>
  <span>启动延迟 <input id="delay" type="number" value="0" min="0" max="60"> 秒</span>
</div>
<div id="out"></div>
<script>
var busy=false, rec=false, lastAt='';
function $(id){return document.getElementById(id)}
function esc(s){return String(s==null?'':s).replace(/[&<>"']/g,function(c){return {'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]})}
function setButtons(){ $('start').disabled = busy || rec; $('stop').disabled = busy || !rec; }
async function j(url){
  const r = await fetch(url,{method:'POST'});
  const d = await r.json().catch(function(){return {}});
  if(!r.ok) throw new Error(d.error || ('HTTP '+r.status));
  return d;
}
function renderReport(rep){
  if(!rep) return;
  let h = '<p>最近指令：<b>'+esc(rep.command)+'</b> @ '+esc(rep.at)+
          ' ｜ 派发差 '+Number(rep.spread_ms).toFixed(2)+' ms ｜ 失败 '+rep.failed+'</p>';
  if(rep.clock_note) h += '<p class="clock">⏱ '+esc(rep.clock_note)+'</p>';
  h += '<table><tr><th>相机</th><th>派发偏移</th><th>RTT</th><th>结果</th></tr>';
  for(const r of (rep.rows||[])){
    h += '<tr><td>'+esc(r.name)+'<div class="cam">'+esc(r.addr)+'</div></td>'+
         '<td>+'+Number(r.dispatch_ms).toFixed(2)+' ms</td>'+
         '<td>'+Number(r.rtt_ms).toFixed(1)+' ms</td>'+
         '<td class="'+(r.ok?'ok':'err')+'">'+(r.ok?'✓':'✗ '+esc(r.error))+
         (r.file?'<div class="cam">'+esc(r.file)+'</div>':'')+'</td></tr>';
  }
  h += '</table>';
  $('out').innerHTML = h;
}
async function doStart(){
  if(busy) return;
  busy=true; setButtons();
  try{
    const d = parseInt($('delay').value || '0',10)||0;
    renderReport(await j('/api/start?delay_ms='+(d*1000)));
  }catch(e){ alert('开始失败: '+e.message); }
  busy=false; refresh();
}
async function doStop(){
  if(busy) return;
  busy=true; setButtons();
  try{ renderReport(await j('/api/stop')); }
  catch(e){ alert('停止失败: '+e.message); }
  busy=false; refresh();
}
async function act(url,label){
  try{ renderReport(await j(url)); }
  catch(e){ alert(label+'失败: '+e.message); }
  refresh();
}
async function refresh(){
  try{
    const s = await (await fetch('/api/status')).json();
    if(!busy) rec = !!s.recording;
    const cams = (s.cameras||[]).map(function(c){
      return '📷 '+esc(c.name)+' <span class="cam">'+esc(c.addr)+' · '+esc(c.model)+'</span>';
    });
    $('banner').innerHTML = cams.length ? cams.join(' ｜ ') : '未连接相机';
    setButtons();
    if(s.last && s.last.at !== lastAt){ lastAt = s.last.at; renderReport(s.last); }
  }catch(e){
    $('banner').textContent = '服务器未响应：'+e.message;
  }
}
refresh();
setInterval(refresh, 1000);
</script>
</body>
</html>
`
