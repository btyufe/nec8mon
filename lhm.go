package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// lhmNode 对应 LibreHardwareMonitor v0.9.6 的 data.json 节点。
// 字段取自 LibreHardwareMonitor/Utilities/HttpServer.cs 的 GenerateJsonForNode。
type lhmNode struct {
	ID         int       `json:"id"`
	Text       string    `json:"Text"`
	Min        string    `json:"Min"`
	Value      string    `json:"Value"`
	Max        string    `json:"Max"`
	RawMin     string    `json:"RawMin"`
	RawValue   string    `json:"RawValue"`
	RawMax     string    `json:"RawMax"`
	Type       string    `json:"Type"`
	SensorID   string    `json:"SensorId"`
	HardwareID string    `json:"HardwareId"`
	Children   []lhmNode `json:"Children"`
}

// lhmSource 通过 LibreHardwareMonitor 自带的 HTTP 接口读取传感器。
// 数据地址： http://<ip>:<port>/data.json
type lhmSource struct {
	baseURL string
	client  *http.Client
}

// newLHMSource 创建一个 LHM HTTP 数据源。
func newLHMSource(baseURL string) *lhmSource {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8085"
	}
	return &lhmSource{
		baseURL: baseURL,
		client:  &http.Client{Timeout: 5 * time.Second},
	}
}

func (s *lhmSource) name() string { return "librehardwaremonitor" }

func (s *lhmSource) collect(ctx context.Context) ([]Reading, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.baseURL+"/data.json", nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	var root lhmNode
	if err := json.NewDecoder(resp.Body).Decode(&root); err != nil {
		return nil, fmt.Errorf("解析 data.json 失败: %w", err)
	}

	var out []Reading
	walkLHM(&root, "", &out)
	if len(out) == 0 {
		return nil, fmt.Errorf("data.json 中没有传感器节点")
	}
	return out, nil
}

// walkLHM 递归遍历传感器树，同时记录每个传感器所属的硬件名称。
func walkLHM(n *lhmNode, hardware string, out *[]Reading) {
	name := strings.TrimSpace(n.Text)

	// 硬件节点带 HardwareId 但没有 SensorId
	if n.HardwareID != "" && n.SensorID == "" {
		hardware = name
	}

	// 传感器节点带 SensorId
	if n.SensorID != "" && n.Type != "" {
		if v, unit, ok := parseValue(n.RawValue); ok {
			if unit == "" {
				unit = unitForCategory(n.Type)
			}
			minV, _, _ := parseValue(n.RawMin)
			maxV, _, _ := parseValue(n.RawMax)
			*out = append(*out, Reading{
				ID:       n.SensorID,
				Name:     name,
				Category: n.Type,
				Hardware: hardware,
				Value:    v,
				Unit:     unit,
				Min:      minV,
				Max:      maxV,
				Source:   "librehardwaremonitor",
			})
		}
	}

	for i := range n.Children {
		walkLHM(&n.Children[i], hardware, out)
	}
}
