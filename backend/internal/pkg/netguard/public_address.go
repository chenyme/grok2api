// Package netguard 提供所有服务端远程抓取共享的网络地址安全判定。
package netguard

import "net/netip"

var blockedPublicFetchPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	// IPv6 转换/特殊用途网段可封装 IPv4 或依赖本地转换网关，服务端抓取默认拒绝。
	netip.MustParsePrefix("64:ff9b::/96"), netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"), netip.MustParsePrefix("5f00::/16"),
}

// IsPublicAddress 仅允许可从公网路由且不属于私有、链路本地、多播或特殊用途地址段的地址。
func IsPublicAddress(address netip.Addr) bool {
	address = address.Unmap()
	if !address.IsValid() || !address.IsGlobalUnicast() || address.IsPrivate() || address.IsLoopback() ||
		address.IsLinkLocalUnicast() || address.IsMulticast() || address.IsUnspecified() || address.IsInterfaceLocalMulticast() {
		return false
	}
	for _, prefix := range blockedPublicFetchPrefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}
