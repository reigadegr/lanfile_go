package main

import (
	"net"
	"strings"
	"sync"
	"time"
)

var (
	vpnInterfacePrefixes = []string{"tun", "tap", "docker", "veth", "br-", "wg", "ppp", "utun", "vir", "vgate"}
	lanInterfacePrefixes = []string{"wlan", "eth", "en", "usb"}
)

// lanIPCache 缓存最近一次探测结果 1 秒，避免每个列表请求都枚举一遍网卡。
type lanIPCache struct {
	mu        sync.Mutex
	ip        string
	fetchedAt time.Time
}

// get 返回局域网 IP；探测不到时返回空串，前端据此隐藏局域网直链。
func (c *lanIPCache) get() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.fetchedAt.IsZero() && time.Since(c.fetchedAt) < time.Second {
		return c.ip
	}
	c.ip = detectLANIP()
	c.fetchedAt = time.Now()
	return c.ip
}

// detectLANIP 优先选 wlan/eth/en/usb 网卡上的 IPv4，其次任意非 VPN 网卡。
func detectLANIP() string {
	interfaces, err := net.Interfaces()
	if err != nil {
		return ""
	}

	fallback := ""
	for _, iface := range interfaces {
		ip := lanIPCandidate(iface)
		if ip == "" {
			continue
		}
		if hasInterfacePrefix(iface.Name, lanInterfacePrefixes) {
			return ip
		}
		if fallback == "" {
			fallback = ip
		}
	}
	return fallback
}

// lanIPCandidate 返回网卡上适合分享的 IPv4 地址，没有则返回空串。
func lanIPCandidate(iface net.Interface) string {
	if hasInterfacePrefix(iface.Name, vpnInterfacePrefixes) {
		return ""
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return ""
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipNet.IP.To4()
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			continue
		}
		// 169.254.0.0/16 是链路本地地址，对端无法直接访问
		if ip[0] == 169 && ip[1] == 254 {
			continue
		}
		// /32 掩码通常是点对点隧道（如 VPN），不是局域网
		if ones, bits := ipNet.Mask.Size(); bits == 32 && ones == 32 {
			continue
		}
		return ip.String()
	}
	return ""
}

// hasInterfacePrefix 判断网卡名是否以任一前缀开头（忽略 ASCII 大小写）。
func hasInterfacePrefix(name string, prefixes []string) bool {
	for _, prefix := range prefixes {
		if len(name) >= len(prefix) && strings.EqualFold(name[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}
