package relay

import (
	"fmt"
	"html/template"
	"net/http"
)

var dashboard = template.Must(template.New("relay").Funcs(template.FuncMap{
	"duration": func(d DurationStats) string {
		if d.AverageMS == nil {
			return "暂无样本"
		}
		return fmt.Sprintf("%.2f ms", *d.AverageMS)
	},
}).Parse(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta http-equiv="refresh" content="5"><title>Relay 耗时统计</title>
<style>body{margin:0;background:#f4f6f9;color:#182334;font:16px/1.6 system-ui,sans-serif}main{max-width:1040px;margin:8vh auto;padding:24px}h1{margin-bottom:4px}p{color:#59667a}.metrics{display:grid;grid-template-columns:repeat(3,minmax(0,1fr));gap:16px;margin:32px 0}.card{background:white;border:1px solid #dce2eb;border-radius:14px;padding:24px}.label{font-weight:600}.value{font-size:30px;font-weight:700;margin:14px 0}.description{font-size:14px;color:#59667a}@media(max-width:760px){.metrics{grid-template-columns:1fr}}</style></head>
<body><main><h1>Relay 耗时统计</h1><p>最近 {{.Samples}} / {{.WindowSize}} 个收到首 token 的模型请求 · 每 5 秒刷新</p><section class="metrics">
<div class="card"><div class="label">平均上传耗时</div><div class="value">{{duration .Upload}}</div><div class="description">中转开始接收请求 → 请求体接收完毕</div></div>
<div class="card"><div class="label">平均本地处理耗时</div><div class="value">{{duration .Preparation}}</div><div class="description">请求体接收完毕 → 解压和 HTTP 请求构造完成</div></div>
<div class="card"><div class="label">平均火山首 token 耗时</div><div class="value">{{duration .FirstToken}}</div><div class="description">HTTP 请求构造完成 → 收到火山首个有效模型增量</div></div>
</section><p>三个阶段互斥，相加为中转开始接收请求到首 token 的平均总耗时。没有收到首 token 的请求不计入窗口；重启后重新累计。</p></main></body></html>`))

// The standalone process binds this handler to loopback port 10800 by default.
func (h *Handler) StatsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay" && r.URL.Path != "/relay/" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = dashboard.Execute(w, h.Timings.Snapshot())
	})
}
