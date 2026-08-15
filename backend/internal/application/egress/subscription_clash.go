package egress

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type clashSubscription struct {
	Proxies []yaml.Node `yaml:"proxies"`
}

type clashProxy struct {
	Type              string               `yaml:"type"`
	Server            string               `yaml:"server"`
	Port              clashInteger         `yaml:"port"`
	Username          string               `yaml:"username"`
	Password          string               `yaml:"password"`
	Cipher            string               `yaml:"cipher"`
	UUID              string               `yaml:"uuid"`
	AlterID           clashInteger         `yaml:"alterId"`
	Network           string               `yaml:"network"`
	TLS               bool                 `yaml:"tls"`
	ServerName        string               `yaml:"servername"`
	SNI               string               `yaml:"sni"`
	Flow              string               `yaml:"flow"`
	ALPN              clashStringList      `yaml:"alpn"`
	SkipCertVerify    bool                 `yaml:"skip-cert-verify"`
	ClientFingerprint string               `yaml:"client-fingerprint"`
	Plugin            string               `yaml:"plugin"`
	WSOptions         clashWebSocketOption `yaml:"ws-opts"`
	RealityOptions    clashRealityOptions  `yaml:"reality-opts"`
}

type clashWebSocketOption struct {
	Path    string            `yaml:"path"`
	Headers map[string]string `yaml:"headers"`
}

type clashRealityOptions struct {
	PublicKey string `yaml:"public-key"`
	ShortID   string `yaml:"short-id"`
}

type clashInteger int

func (value *clashInteger) UnmarshalYAML(node *yaml.Node) error {
	if node == nil || node.Kind != yaml.ScalarNode {
		return errors.New("Clash 整数字段格式无效")
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(node.Value), 10, 32)
	if err != nil {
		return errors.New("Clash 整数字段格式无效")
	}
	*value = clashInteger(parsed)
	return nil
}

type clashStringList []string

func (value *clashStringList) UnmarshalYAML(node *yaml.Node) error {
	if node == nil {
		return errors.New("Clash 字符串列表格式无效")
	}
	if node.Kind == yaml.AliasNode {
		if node.Alias == nil {
			return errors.New("Clash 字符串列表格式无效")
		}
		node = node.Alias
	}
	var items []string
	switch node.Kind {
	case yaml.ScalarNode:
		items = strings.Split(node.Value, ",")
	case yaml.SequenceNode:
		if err := node.Decode(&items); err != nil {
			return errors.New("Clash 字符串列表格式无效")
		}
	default:
		return errors.New("Clash 字符串列表格式无效")
	}
	normalized := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			normalized = append(normalized, item)
		}
	}
	*value = normalized
	return nil
}

type clashVMessShare struct {
	Version       string `json:"v"`
	Address       string `json:"add"`
	Port          string `json:"port"`
	UUID          string `json:"id"`
	AlterID       string `json:"aid"`
	Cipher        string `json:"scy"`
	Network       string `json:"net"`
	TLS           string `json:"tls,omitempty"`
	ServerName    string `json:"sni,omitempty"`
	ALPN          string `json:"alpn,omitempty"`
	Host          string `json:"host,omitempty"`
	Path          string `json:"path,omitempty"`
	AllowInsecure bool   `json:"allowInsecure,omitempty"`
}

func parseClashSubscription(value string) ([]subscriptionEntry, int, bool) {
	var document clashSubscription
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(value, "\ufeff")), &document); err != nil || document.Proxies == nil {
		return nil, 0, false
	}
	if len(document.Proxies) > maxSubscriptionEntries {
		return nil, len(document.Proxies), true
	}
	lines := make([]string, 0, len(document.Proxies))
	skipped := 0
	for index := range document.Proxies {
		var proxy clashProxy
		if err := document.Proxies[index].Decode(&proxy); err != nil {
			skipped++
			continue
		}
		line, err := clashProxyURL(proxy)
		if err != nil {
			skipped++
			continue
		}
		lines = append(lines, line)
	}
	entries, lineSkipped := parseProxyLines(strings.Join(lines, "\n"))
	return entries, skipped + lineSkipped, true
}

func clashProxyURL(proxy clashProxy) (string, error) {
	server, err := clashProxyServer(proxy.Server, int(proxy.Port))
	if err != nil {
		return "", err
	}
	proxyType := strings.ToLower(strings.TrimSpace(proxy.Type))
	switch proxyType {
	case "http":
		scheme := "http"
		if proxy.TLS {
			scheme = "https"
		}
		return clashStandardProxyURL(scheme, server, proxy.Username, proxy.Password), nil
	case "socks5":
		if proxy.TLS {
			return "", errors.New("暂不支持 TLS SOCKS5")
		}
		return clashStandardProxyURL("socks5", server, proxy.Username, proxy.Password), nil
	case "ss":
		if strings.TrimSpace(proxy.Plugin) != "" {
			return "", errors.New("暂不支持 Shadowsocks plugin")
		}
		method := strings.ToLower(strings.TrimSpace(proxy.Cipher))
		if method == "" || proxy.Password == "" {
			return "", errors.New("Shadowsocks cipher/password 不能为空")
		}
		credential := base64.RawURLEncoding.EncodeToString([]byte(method + ":" + proxy.Password))
		return "ss://" + credential + "@" + server, nil
	case "trojan":
		if proxy.Password == "" {
			return "", errors.New("Trojan password 不能为空")
		}
		query, err := clashTunnelQuery(proxy, "tls")
		if err != nil {
			return "", err
		}
		return (&url.URL{Scheme: "trojan", User: url.User(proxy.Password), Host: server, RawQuery: query.Encode()}).String(), nil
	case "vless":
		if proxy.UUID == "" {
			return "", errors.New("VLESS UUID 不能为空")
		}
		security := "none"
		if proxy.RealityOptions.PublicKey != "" || proxy.RealityOptions.ShortID != "" {
			security = "reality"
		} else if proxy.TLS {
			security = "tls"
		}
		query, err := clashTunnelQuery(proxy, security)
		if err != nil {
			return "", err
		}
		query.Set("encryption", "none")
		if flow := strings.TrimSpace(proxy.Flow); flow != "" {
			query.Set("flow", flow)
		}
		if security == "reality" {
			query.Set("pbk", proxy.RealityOptions.PublicKey)
			query.Set("sid", proxy.RealityOptions.ShortID)
			query.Set("fp", firstNonEmptySubscription(proxy.ClientFingerprint, "chrome"))
		}
		return (&url.URL{Scheme: "vless", User: url.User(proxy.UUID), Host: server, RawQuery: query.Encode()}).String(), nil
	case "vmess":
		return clashVMessURL(proxy)
	default:
		return "", fmt.Errorf("暂不支持 Clash 代理类型 %q", proxyType)
	}
}

func clashProxyServer(host string, port int) (string, error) {
	host = strings.Trim(strings.TrimSpace(host), "[]")
	if host == "" || port < 1 || port > 65535 {
		return "", errors.New("Clash 代理服务器地址无效")
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func clashStandardProxyURL(scheme, server, username, password string) string {
	value := &url.URL{Scheme: scheme, Host: server}
	if username != "" || password != "" {
		value.User = url.UserPassword(username, password)
	}
	return value.String()
}

func clashTunnelQuery(proxy clashProxy, security string) (url.Values, error) {
	transport := strings.ToLower(strings.TrimSpace(proxy.Network))
	if transport == "" {
		transport = "tcp"
	}
	if transport == "websocket" {
		transport = "ws"
	}
	query := make(url.Values)
	query.Set("type", transport)
	query.Set("security", security)
	query.Set("sni", firstNonEmptySubscription(proxy.ServerName, proxy.SNI, proxy.Server))
	if proxy.SkipCertVerify {
		query.Set("allowInsecure", "1")
	}
	if len(proxy.ALPN) != 0 {
		query.Set("alpn", strings.Join(proxy.ALPN, ","))
	}
	if transport == "ws" {
		query.Set("path", firstNonEmptySubscription(proxy.WSOptions.Path, "/"))
		query.Set("host", firstNonEmptySubscription(proxy.WSOptions.Headers["Host"], proxy.WSOptions.Headers["host"], proxy.ServerName, proxy.SNI, proxy.Server))
	}
	return query, nil
}

func clashVMessURL(proxy clashProxy) (string, error) {
	if proxy.UUID == "" {
		return "", errors.New("VMess UUID 不能为空")
	}
	network := strings.ToLower(strings.TrimSpace(proxy.Network))
	if network == "" {
		network = "tcp"
	}
	if network == "websocket" {
		network = "ws"
	}
	share := clashVMessShare{
		Version: "2", Address: proxy.Server, Port: strconv.Itoa(int(proxy.Port)), UUID: proxy.UUID,
		AlterID: strconv.Itoa(int(proxy.AlterID)), Cipher: firstNonEmptySubscription(proxy.Cipher, "auto"), Network: network,
		ServerName: firstNonEmptySubscription(proxy.ServerName, proxy.SNI, proxy.Server), ALPN: strings.Join(proxy.ALPN, ","),
		AllowInsecure: proxy.SkipCertVerify,
	}
	if proxy.TLS {
		share.TLS = "tls"
	}
	if network == "ws" {
		share.Path = firstNonEmptySubscription(proxy.WSOptions.Path, "/")
		share.Host = firstNonEmptySubscription(proxy.WSOptions.Headers["Host"], proxy.WSOptions.Headers["host"], proxy.ServerName, proxy.SNI, proxy.Server)
	}
	encoded, err := json.Marshal(share)
	if err != nil {
		return "", err
	}
	return "vmess://" + base64.RawStdEncoding.EncodeToString(encoded), nil
}

func firstNonEmptySubscription(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
