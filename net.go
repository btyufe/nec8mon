package main

import (
	"net"
	"strings"
)

// localIPv4 返回本机可用的 IPv4 地址（排除回环与链路本地）。
func localIPv4() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		ipNet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			continue
		}
		if ip.To4() == nil {
			continue
		}
		out = append(out, ip.String())
	}
	return out
}

// normalizeAddr 确保监听地址带端口。
func normalizeAddr(addr string) string {
	if addr == "" {
		return ":8080"
	}
	if strings.Contains(addr, ":") {
		return addr
	}
	return ":" + addr
}
