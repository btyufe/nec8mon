package main

import (
	"context"
	"embed"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"
)

//go:embed web
var webFS embed.FS

// server 暴露 JSON API 与内置仪表盘。
type server struct {
	collector *Collector
	fan       *FanController
	started   time.Time
}

// newServer 创建 HTTP 服务处理器。
func newServer(c *Collector, fan *FanController) *server {
	return &server{collector: c, fan: fan, started: time.Now()}
}

// routes 返回路由表。
func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/sensors", s.handleSensors)
	mux.HandleFunc("/api/history", s.handleHistory)
	mux.HandleFunc("/api/health", s.handleHealth)
	mux.HandleFunc("/api/fan", s.handleFanState)
	mux.HandleFunc("/api/fan/control", s.handleFanControl)
	mux.HandleFunc("/api/fan/test", s.handleFanTest)
	mux.HandleFunc("/", s.handleIndex)

	return logRequests(mux)
}

// fanControlRequest 是调速指令的请求体。字段都是指针，
// 这样"没传的字段"和"传了 0"能被区分开。
type fanControlRequest struct {
	Mode       *string   `json:"mode"`
	Duty       *float64  `json:"duty"`
	Curve      *FanCurve `json:"curve"`
	SafetyTemp *float64  `json:"safetyTemp"`
	Channel    *int      `json:"channel"`
}

// handleFanState 返回风扇控制状态。
func (s *server) handleFanState(w http.ResponseWriter, r *http.Request) {
	if s.fan == nil {
		writeJSON(w, http.StatusOK, FanState{})
		return
	}
	writeJSON(w, http.StatusOK, s.fan.State())
}

// handleFanControl 应用调速指令。
func (s *server) handleFanControl(w http.ResponseWriter, r *http.Request) {
	if s.fan == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "调速功能不可用"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "请使用 POST"})
		return
	}

	var req fanControlRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
		return
	}

	// 先应用通道选择，再下发模式与占空比，避免先把旧通道切成手动
	if req.Channel != nil {
		if err := s.fan.SetChannel(*req.Channel); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.Mode != nil {
		if err := s.fan.SetMode(FanMode(*req.Mode)); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.Curve != nil {
		if err := s.fan.SetCurve(*req.Curve); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.Duty != nil {
		if err := s.fan.SetDuty(*req.Duty); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	if req.SafetyTemp != nil {
		if err := s.fan.SetSafetyTemp(*req.SafetyTemp); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}

	writeJSON(w, http.StatusOK, s.fan.State())
}

// fanTestRequest 是通道自检测试请求体。
type fanTestRequest struct {
	Channel  int     `json:"channel"`
	Duty     float64 `json:"duty"`
	Duration int     `json:"duration"` // 秒数，默认 6 秒
}

// handleFanTest 执行指定通道的短暂自检测试。
func (s *server) handleFanTest(w http.ResponseWriter, r *http.Request) {
	if s.fan == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "调速功能不可用"})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "请使用 POST"})
		return
	}

	var req fanTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "请求格式错误: " + err.Error()})
		return
	}

	duty := req.Duty
	if duty <= 0 {
		duty = 50
	}
	durSec := req.Duration
	if durSec <= 0 || durSec > 30 {
		durSec = 6
	}

	if err := s.fan.TestChannel(req.Channel, duty, time.Duration(durSec)*time.Second); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"channel": req.Channel,
		"duty":    duty,
		"seconds": durSec,
		"state":   s.fan.State(),
	})
}

// handleSensors 返回当前完整快照。
func (s *server) handleSensors(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.collector.Snapshot())
}

// handleHistory 返回历史采样点，供前端画趋势图。
func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.collector.History())
}

// handleHealth 返回存活状态。任一数据源可用即视为健康。
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	snap := s.collector.Snapshot()
	healthy := false
	for _, st := range snap.Sources {
		if st.OK && st.Count > 0 {
			healthy = true
			break
		}
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{
		"healthy":       healthy,
		"hostname":      snap.Hostname,
		"uptimeSeconds": time.Since(s.started).Seconds(),
		"updatedAt":     snap.UpdatedAt,
	})
}

// handleIndex 返回内置的单页仪表盘。
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	page, err := webFS.ReadFile("web/index.html")
	if err != nil {
		http.Error(w, "仪表盘资源缺失", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(page)
}

// writeJSON 以 JSON 格式写出响应。
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("[api] 写出 JSON 失败: %v", err)
	}
}

// statusRecorder 记录响应状态码，用于访问日志。
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// logRequests 给每个请求加一行精简访问日志。
func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		log.Printf("[http] %s %s %d %s", r.Method, r.URL.Path, rec.status,
			strconv.FormatInt(time.Since(start).Milliseconds(), 10)+"ms")
	})
}

// runServer 启动 HTTP 服务并阻塞直到 ctx 取消。
func runServer(ctx context.Context, addr string, c *Collector, fan *FanController) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           newServer(c, fan).routes(),
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("[http] 监听 %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutCtx)
	}
}
