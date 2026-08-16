package tunnelproxy

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Asutorufa/yuhaiin/pkg/net/netapi"
	"github.com/Asutorufa/yuhaiin/pkg/net/proxy/trojan"
	visionproxy "github.com/Asutorufa/yuhaiin/pkg/net/proxy/vision"
	yuhaiinvless "github.com/Asutorufa/yuhaiin/pkg/net/proxy/vless"
	"github.com/Asutorufa/yuhaiin/pkg/net/proxy/vmess"
	"github.com/Asutorufa/yuhaiin/pkg/protos/node/protocol"
	"github.com/coder/websocket"
	"github.com/google/uuid"
	singvmess "github.com/metacubex/sing-vmess"
	singvless "github.com/metacubex/sing-vmess/vless"
	singmetadata "github.com/metacubex/sing/common/metadata"
	sscore "github.com/shadowsocks/go-shadowsocks2/core"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type Config struct {
	Scheme            string
	Server            string
	Credential        string
	Method            string
	AlterID           int
	Cipher            string
	Transport         string
	TLS               bool
	Security          string
	Flow              string
	ServerName        string
	Insecure          bool
	ALPN              []string
	RealityPublicKey  string
	RealityShortID    string
	ClientFingerprint string
	SpiderX           string
	WebSocketHost     string
	WebSocketPath     string
	CanonicalProxyURL string
}

const tunnelHandshakeTimeout = 10 * time.Second

func IsSupportedScheme(scheme string) bool {
	switch strings.ToLower(strings.TrimSpace(scheme)) {
	case "trojan", "vless", "ss", "vmess":
		return true
	default:
		return false
	}
}

func Normalize(value string) (string, error) {
	config, err := Parse(value)
	if err != nil {
		return "", err
	}
	return config.CanonicalProxyURL, nil
}

func Parse(value string) (Config, error) {
	value = strings.TrimSpace(value)
	if strings.IndexFunc(value, func(character rune) bool { return character < 0x20 || character == 0x7f }) >= 0 {
		return Config{}, errors.New("隧道代理地址包含控制字符")
	}
	scheme, _, ok := strings.Cut(value, "://")
	if !ok || !IsSupportedScheme(scheme) {
		return Config{}, errors.New("不支持的隧道代理协议")
	}
	switch strings.ToLower(scheme) {
	case "ss":
		return parseShadowsocks(value)
	case "vmess":
		return parseVMess(value)
	default:
		return parseUserInfoProxy(value)
	}
}

func parseUserInfoProxy(value string) (Config, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || parsed.User == nil {
		return Config{}, errors.New("隧道代理地址格式无效")
	}
	scheme := strings.ToLower(parsed.Scheme)
	credential := parsed.User.Username()
	if credential == "" {
		return Config{}, errors.New("隧道代理凭据不能为空")
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		return Config{}, errors.New("隧道代理凭据格式无效")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return Config{}, errors.New("隧道代理地址路径无效")
	}
	server, err := canonicalServer(parsed.Hostname(), parsed.Port())
	if err != nil {
		return Config{}, err
	}
	query := parsed.Query()
	transport := strings.ToLower(firstNonEmpty(query.Get("type"), query.Get("network")))
	if transport == "" || transport == "none" {
		transport = "tcp"
	}
	if transport == "websocket" {
		transport = "ws"
	}
	if transport != "tcp" && transport != "ws" {
		return Config{}, fmt.Errorf("暂不支持 %s 传输", transport)
	}
	flow := strings.ToLower(strings.TrimSpace(query.Get("flow")))
	if scheme == "vless" {
		parsedUUID, err := uuid.Parse(credential)
		if err != nil {
			return Config{}, errors.New("VLESS UUID 无效")
		}
		credential = parsedUUID.String()
		if encryption := strings.ToLower(strings.TrimSpace(query.Get("encryption"))); encryption != "" && encryption != "none" {
			return Config{}, errors.New("VLESS encryption 必须为 none")
		}
	}
	security := strings.ToLower(strings.TrimSpace(query.Get("security")))
	tlsEnabled := scheme == "trojan"
	securityMode := "none"
	if tlsEnabled {
		securityMode = "tls"
	}
	switch security {
	case "", "tls":
		if security == "tls" {
			tlsEnabled = true
			securityMode = "tls"
		}
	case "none":
		tlsEnabled = false
		securityMode = "none"
	case "reality":
		if scheme != "vless" {
			return Config{}, errors.New("Reality security 当前仅支持 VLESS")
		}
		if transport != "tcp" {
			return Config{}, errors.New("VLESS Reality 当前仅支持 TCP 传输")
		}
		tlsEnabled = true
		securityMode = "reality"
	default:
		return Config{}, fmt.Errorf("暂不支持 %s security %q", strings.ToUpper(scheme), security)
	}
	if flow != "" {
		if scheme != "vless" || flow != "xtls-rprx-vision" {
			return Config{}, fmt.Errorf("暂不支持 VLESS flow %q", flow)
		}
		if transport != "tcp" || (securityMode != "tls" && securityMode != "reality") {
			return Config{}, errors.New("VLESS Vision 需要 TCP 和 TLS 或 Reality security")
		}
	}
	if headerType := strings.ToLower(strings.TrimSpace(query.Get("headerType"))); headerType != "" && headerType != "none" {
		return Config{}, fmt.Errorf("暂不支持 %s headerType %q", strings.ToUpper(scheme), headerType)
	}
	insecure, err := queryBool(query, "allowInsecure", "insecure", "skip-cert-verify")
	if err != nil {
		return Config{}, err
	}
	serverName := firstNonEmpty(query.Get("sni"), query.Get("peer"), parsed.Hostname())
	realityPublicKey := firstNonEmpty(query.Get("pbk"), query.Get("public-key"), query.Get("publicKey"))
	realityShortID := firstNonEmpty(query.Get("sid"), query.Get("short-id"), query.Get("shortId"))
	fingerprint := strings.ToLower(firstNonEmpty(query.Get("fp"), query.Get("fingerprint"), query.Get("client-fingerprint")))
	spiderX := firstNonEmpty(query.Get("spx"), query.Get("spider-x"), query.Get("spiderX"))
	if securityMode == "reality" {
		if realityPublicKey == "" {
			return Config{}, errors.New("VLESS Reality public key 不能为空")
		}
		if fingerprint == "" {
			fingerprint = "chrome"
		}
		if _, supported := realityClientHelloID(fingerprint); !supported {
			return Config{}, fmt.Errorf("VLESS Reality 暂不支持客户端指纹 %q", fingerprint)
		}
	} else if realityPublicKey != "" || realityShortID != "" || fingerprint != "" || spiderX != "" {
		return Config{}, errors.New("Reality 参数只能用于 Reality security")
	}
	wsHost := firstNonEmpty(query.Get("host"), serverName)
	if err := validateWebSocketHost(wsHost); transport == "ws" && err != nil {
		return Config{}, err
	}
	wsPath := query.Get("path")
	if wsPath == "" {
		wsPath = "/"
	} else if !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}
	if transport == "tcp" && (query.Get("path") != "" || query.Get("host") != "") {
		return Config{}, errors.New("TCP 隧道不能包含 WebSocket host/path")
	}
	config := Config{
		Scheme: scheme, Server: server, Credential: credential, Transport: transport,
		TLS: tlsEnabled, Security: securityMode, Flow: flow, ServerName: serverName, Insecure: insecure,
		ALPN: splitList(query.Get("alpn")), RealityPublicKey: realityPublicKey, RealityShortID: realityShortID,
		ClientFingerprint: fingerprint, SpiderX: spiderX, WebSocketHost: wsHost, WebSocketPath: wsPath,
	}
	config.CanonicalProxyURL = canonicalUserInfoProxyURL(config)
	if err := validateConfig(config); err != nil {
		return Config{}, fmt.Errorf("创建 %s 隧道配置: %w", strings.ToUpper(scheme), err)
	}
	return config, nil
}

func canonicalUserInfoProxyURL(config Config) string {
	query := make(url.Values)
	query.Set("type", config.Transport)
	security := config.Security
	if security == "" {
		security = "none"
		if config.TLS {
			security = "tls"
		}
	}
	query.Set("security", security)
	if config.Scheme == "vless" {
		query.Set("encryption", "none")
	}
	if config.Flow != "" {
		query.Set("flow", config.Flow)
	}
	query.Set("sni", config.ServerName)
	if security == "reality" {
		query.Set("pbk", config.RealityPublicKey)
		query.Set("sid", config.RealityShortID)
		query.Set("fp", config.ClientFingerprint)
		if config.SpiderX != "" {
			query.Set("spx", config.SpiderX)
		}
	}
	if config.Insecure {
		query.Set("allowInsecure", "1")
	}
	if len(config.ALPN) != 0 {
		query.Set("alpn", strings.Join(config.ALPN, ","))
	}
	if config.Transport == "ws" {
		query.Set("host", config.WebSocketHost)
		query.Set("path", config.WebSocketPath)
	}
	return (&url.URL{
		Scheme:   config.Scheme,
		User:     url.User(config.Credential),
		Host:     config.Server,
		RawQuery: query.Encode(),
	}).String()
}

func parseShadowsocks(value string) (Config, error) {
	payload := value[len("ss://"):]
	if before, _, found := strings.Cut(payload, "#"); found {
		payload = before
	}
	if before, rawQuery, found := strings.Cut(payload, "?"); found {
		query, err := url.ParseQuery(rawQuery)
		if err != nil {
			return Config{}, errors.New("Shadowsocks 查询参数无效")
		}
		if plugin := strings.TrimSpace(query.Get("plugin")); plugin != "" {
			return Config{}, errors.New("暂不支持 Shadowsocks plugin")
		}
		if len(query) != 0 {
			return Config{}, errors.New("Shadowsocks 查询参数无效")
		}
		payload = before
	}
	var userPart, serverPart string
	if separator := strings.LastIndex(payload, "@"); separator >= 0 {
		userPart, serverPart = payload[:separator], payload[separator+1:]
		decoded, err := url.PathUnescape(userPart)
		if err != nil {
			return Config{}, errors.New("Shadowsocks 凭据无效")
		}
		if !strings.Contains(decoded, ":") {
			decoded, err = decodeBase64String(decoded)
			if err != nil {
				return Config{}, errors.New("Shadowsocks 凭据不是有效的 Base64")
			}
		}
		userPart = decoded
	} else {
		decoded, err := decodeBase64String(payload)
		if err != nil {
			return Config{}, errors.New("旧式 Shadowsocks 地址不是有效的 Base64")
		}
		separator := strings.LastIndex(decoded, "@")
		if separator < 0 {
			return Config{}, errors.New("旧式 Shadowsocks 地址格式无效")
		}
		userPart, serverPart = decoded[:separator], decoded[separator+1:]
	}
	method, password, ok := strings.Cut(userPart, ":")
	if !ok || strings.TrimSpace(method) == "" || password == "" {
		return Config{}, errors.New("Shadowsocks method/password 不能为空")
	}
	method = strings.ToLower(strings.TrimSpace(method))
	switch method {
	case "aes-128-gcm", "aes-256-gcm", "chacha20-ietf-poly1305":
	default:
		return Config{}, fmt.Errorf("暂不支持 Shadowsocks method %q", method)
	}
	serverURL, err := url.Parse("ss://" + serverPart)
	if err != nil || serverURL.User != nil || (serverURL.Path != "" && serverURL.Path != "/") || serverURL.RawQuery != "" || serverURL.Fragment != "" {
		return Config{}, errors.New("Shadowsocks 服务器地址无效")
	}
	server, err := canonicalServer(serverURL.Hostname(), serverURL.Port())
	if err != nil {
		return Config{}, err
	}
	credential := method + ":" + password
	config := Config{
		Scheme: "ss", Server: server, Credential: password, Method: method, Transport: "tcp",
		CanonicalProxyURL: "ss://" + base64.RawURLEncoding.EncodeToString([]byte(credential)) + "@" + server,
	}
	if err := validateConfig(config); err != nil {
		return Config{}, fmt.Errorf("创建 Shadowsocks 隧道配置: %w", err)
	}
	return config, nil
}

type vmessShare struct {
	Version       string `json:"v,omitempty"`
	Address       string `json:"add"`
	Port          string `json:"port"`
	UUID          string `json:"id"`
	AlterID       string `json:"aid"`
	Cipher        string `json:"scy,omitempty"`
	Network       string `json:"net"`
	TLS           string `json:"tls,omitempty"`
	ServerName    string `json:"sni,omitempty"`
	ALPN          string `json:"alpn,omitempty"`
	Host          string `json:"host,omitempty"`
	Path          string `json:"path,omitempty"`
	AllowInsecure bool   `json:"allowInsecure,omitempty"`
}

func parseVMess(value string) (Config, error) {
	payload := value[len("vmess://"):]
	if before, _, found := strings.Cut(payload, "#"); found {
		payload = before
	}
	decoded, err := decodeBase64Bytes(payload)
	if err != nil {
		return Config{}, errors.New("VMess 配置不是有效的 Base64")
	}
	var raw map[string]any
	if err := json.Unmarshal(decoded, &raw); err != nil {
		return Config{}, errors.New("VMess 配置不是有效的 JSON")
	}
	address := jsonString(raw, "add")
	port := jsonString(raw, "port")
	server, err := canonicalServer(address, port)
	if err != nil {
		return Config{}, fmt.Errorf("VMess %w", err)
	}
	parsedUUID, err := uuid.Parse(jsonString(raw, "id"))
	if err != nil {
		return Config{}, errors.New("VMess UUID 无效")
	}
	userID := parsedUUID.String()
	alterID, err := strconv.Atoi(firstNonEmpty(jsonString(raw, "aid"), "0"))
	if err != nil || alterID < 0 || alterID > 65535 {
		return Config{}, errors.New("VMess alterId 无效")
	}
	cipher := strings.ToLower(firstNonEmpty(jsonString(raw, "scy"), jsonString(raw, "security"), "auto"))
	switch cipher {
	case "auto", "aes-128-gcm", "chacha20-poly1305", "none":
	default:
		return Config{}, fmt.Errorf("暂不支持 VMess cipher %q", cipher)
	}
	transport := strings.ToLower(firstNonEmpty(jsonString(raw, "net"), "tcp"))
	if transport == "websocket" {
		transport = "ws"
	}
	if transport != "tcp" && transport != "ws" {
		return Config{}, fmt.Errorf("暂不支持 VMess %s 传输", transport)
	}
	if headerType := strings.ToLower(jsonString(raw, "type")); headerType != "" && headerType != "none" {
		return Config{}, fmt.Errorf("暂不支持 VMess header type %q", headerType)
	}
	tlsMode := strings.ToLower(jsonString(raw, "tls"))
	if tlsMode != "" && tlsMode != "none" && tlsMode != "tls" {
		return Config{}, fmt.Errorf("暂不支持 VMess TLS 模式 %q", tlsMode)
	}
	tlsEnabled := tlsMode == "tls"
	serverName := firstNonEmpty(jsonString(raw, "sni"), address)
	alpn, err := jsonStringList(raw, "alpn")
	if err != nil {
		return Config{}, err
	}
	host := firstNonEmpty(jsonString(raw, "host"), serverName)
	if err := validateWebSocketHost(host); transport == "ws" && err != nil {
		return Config{}, err
	}
	path := jsonString(raw, "path")
	if path == "" {
		path = "/"
	} else if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if transport == "tcp" && (jsonString(raw, "path") != "" || jsonString(raw, "host") != "") {
		return Config{}, errors.New("VMess TCP 隧道不能包含 WebSocket host/path")
	}
	insecure, err := jsonBool(raw, "allowInsecure")
	if err != nil {
		return Config{}, err
	}
	share := vmessShare{
		Version: "2", Address: address, Port: port, UUID: userID, AlterID: strconv.Itoa(alterID), Cipher: cipher,
		Network: transport, ServerName: serverName, ALPN: strings.Join(alpn, ","), AllowInsecure: insecure,
	}
	if transport == "ws" {
		share.Host = host
		share.Path = path
	}
	if tlsEnabled {
		share.TLS = "tls"
	}
	canonicalJSON, err := json.Marshal(share)
	if err != nil {
		return Config{}, err
	}
	config := Config{
		Scheme: "vmess", Server: server, Credential: userID, AlterID: alterID, Cipher: cipher,
		Transport: transport, TLS: tlsEnabled, ServerName: serverName, Insecure: insecure, ALPN: alpn,
		WebSocketHost: host, WebSocketPath: path,
		CanonicalProxyURL: "vmess://" + base64.RawStdEncoding.EncodeToString(canonicalJSON),
	}
	if err := validateConfig(config); err != nil {
		return Config{}, fmt.Errorf("创建 VMess 隧道配置: %w", err)
	}
	return config, nil
}

type Dialer struct {
	proxy             netapi.Proxy
	shadowsocksCipher sscore.StreamConnCipher
	server            string
}

func NewDialer(value string) (*Dialer, error) {
	config, err := Parse(value)
	if err != nil {
		return nil, err
	}
	if config.Scheme == "ss" {
		cipher, err := sscore.PickCipher(config.Method, nil, config.Credential)
		if err != nil {
			return nil, err
		}
		return &Dialer{shadowsocksCipher: cipher, server: config.Server}, nil
	}
	proxy, err := buildProxy(config)
	if err != nil {
		return nil, err
	}
	return &Dialer{proxy: proxy}, nil
}

func (d *Dialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}

func (d *Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil || (d.proxy == nil && d.shadowsocksCipher == nil) {
		return nil, errors.New("隧道代理拨号器未初始化")
	}
	if !strings.HasPrefix(network, "tcp") {
		return nil, fmt.Errorf("隧道代理不支持网络 %q", network)
	}
	if d.shadowsocksCipher != nil {
		return d.dialShadowsocks(ctx, address)
	}
	target, err := netapi.ParseAddress(network, address)
	if err != nil {
		return nil, err
	}
	return d.proxy.Conn(ctx, target)
}

func (d *Dialer) dialShadowsocks(ctx context.Context, address string) (net.Conn, error) {
	target := socks.ParseAddr(address)
	if target == nil {
		return nil, errors.New("Shadowsocks 目标地址无效")
	}
	connection, err := newServerNetDialer().DialContext(ctx, "tcp", d.server)
	if err != nil {
		return nil, err
	}
	connection = d.shadowsocksCipher.StreamConn(connection)
	if _, err := connection.Write(target); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("写入 Shadowsocks 目标地址: %w", err)
	}
	return connection, nil
}

func validateConfig(config Config) error {
	if config.Scheme == "ss" {
		_, err := sscore.PickCipher(config.Method, nil, config.Credential)
		return err
	}
	_, err := buildProxy(config)
	return err
}

func buildProxy(config Config) (netapi.Proxy, error) {
	var current netapi.Proxy = &serverDialer{address: config.Server}
	if config.Transport == "ws" {
		current = &websocketProxy{config: config, dialer: current}
	} else if config.Security == "reality" {
		var err error
		current, err = newRealityProxy(config, current)
		if err != nil {
			return nil, fmt.Errorf("创建 Reality 客户端: %w", err)
		}
	} else if config.TLS {
		current = &tlsProxy{config: config, dialer: current}
	}
	switch config.Scheme {
	case "trojan":
		protocolConfig := &protocol.Trojan{}
		protocolConfig.SetPassword(config.Credential)
		return trojan.NewClient(protocolConfig, current)
	case "vless":
		protocolConfig := &protocol.Vless{}
		protocolConfig.SetUuid(config.Credential)
		return newOwnedVLESSProxy(protocolConfig, current, config.Flow)
	case "vmess":
		protocolConfig := &protocol.Vmess{}
		protocolConfig.SetUuid(config.Credential)
		protocolConfig.SetAlterId(strconv.Itoa(config.AlterID))
		protocolConfig.SetSecurity(config.Cipher)
		return vmess.NewClient(protocolConfig, current)
	default:
		return nil, errors.New("不支持的隧道代理协议")
	}
}

// ownedVLESSProxy closes the transport connection when the upstream VLESS
// client fails while writing its initial request. yuhaiin's VLESS client does
// not transfer the failed connection back to its caller, so ownership must be
// retained at this boundary to prevent failed retries from leaking sockets.
type ownedVLESSProxy struct {
	netapi.EmptyDispatch
	config *protocol.Vless
	dialer netapi.Proxy
	flow   string
	uuid   [16]byte
}

func newOwnedVLESSProxy(config *protocol.Vless, dialer netapi.Proxy, flow string) (netapi.Proxy, error) {
	if _, err := yuhaiinvless.NewClient(config, dialer); err != nil {
		return nil, err
	}
	proxy := &ownedVLESSProxy{config: config, dialer: dialer, flow: flow}
	if flow == "xtls-rprx-vision" {
		userID, err := uuid.Parse(config.GetUuid())
		if err != nil {
			return nil, errors.New("VLESS Vision UUID 无效")
		}
		copy(proxy.uuid[:], userID[:])
	}
	return proxy, nil
}

func (p *ownedVLESSProxy) Conn(ctx context.Context, address netapi.Address) (net.Conn, error) {
	connection, err := p.dialer.Conn(ctx, address)
	if err != nil {
		return nil, err
	}
	if p.flow == "xtls-rprx-vision" {
		result, requestErr := newVisionVLESSConn(connection, address, p.uuid, p.flow)
		if requestErr != nil {
			_ = connection.Close()
			return nil, fmt.Errorf("发送 VLESS Vision 请求: %w", requestErr)
		}
		visionConnection, visionErr := visionproxy.NewVisionConn(result, connection, p.uuid)
		if visionErr != nil {
			_ = result.Close()
			return nil, fmt.Errorf("创建 VLESS Vision 连接: %w", visionErr)
		}
		return visionConnection, nil
	}
	client, err := yuhaiinvless.NewClient(p.config, &singleConnectionProxy{connection: connection})
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	result, err := client.Conn(ctx, address)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}
	return result, nil
}

func newVisionVLESSConn(connection net.Conn, address netapi.Address, userID [16]byte, flow string) (net.Conn, error) {
	destination := singmetadata.ParseSocksaddrHostPort(address.Hostname(), address.Port())
	result := singvless.NewConn(connection, userID, singvmess.CommandTCP, destination, flow)
	if _, err := result.Write(nil); err != nil {
		return nil, err
	}
	return result, nil
}

func (p *ownedVLESSProxy) PacketConn(ctx context.Context, address netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("VLESS 隧道不支持 UDP")
}

type singleConnectionProxy struct {
	netapi.EmptyDispatch
	connection net.Conn
}

func (p *singleConnectionProxy) Conn(context.Context, netapi.Address) (net.Conn, error) {
	if p.connection == nil {
		return nil, errors.New("VLESS 底层连接不可用")
	}
	connection := p.connection
	p.connection = nil
	return connection, nil
}

func (p *singleConnectionProxy) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("VLESS 隧道不支持 UDP")
}

type tlsProxy struct {
	netapi.EmptyDispatch
	config Config
	dialer netapi.Proxy
}

func (p *tlsProxy) Conn(ctx context.Context, address netapi.Address) (net.Conn, error) {
	connection, err := p.dialer.Conn(ctx, address)
	if err != nil {
		return nil, err
	}
	tlsConnection := tls.Client(connection, p.tlsConfig())
	handshakeCtx, cancel := newTunnelHandshakeContext(ctx)
	defer cancel()
	if err := tlsConnection.HandshakeContext(handshakeCtx); err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("隧道 TLS 握手: %w", err)
	}
	return tlsConnection, nil
}

func (p *tlsProxy) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("TLS 隧道不支持 UDP")
}

func (p *tlsProxy) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12, ServerName: p.config.ServerName,
		InsecureSkipVerify: p.config.Insecure, NextProtos: append([]string(nil), p.config.ALPN...),
	}
}

type websocketProxy struct {
	netapi.EmptyDispatch
	config Config
	dialer netapi.Proxy
}

func (p *websocketProxy) Conn(ctx context.Context, address netapi.Address) (net.Conn, error) {
	scheme := "ws"
	if p.config.TLS {
		scheme = "wss"
	}
	path, rawQuery, _ := strings.Cut(p.config.WebSocketPath, "?")
	endpoint := (&url.URL{Scheme: scheme, Host: p.config.WebSocketHost, Path: path, RawQuery: rawQuery}).String()
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return p.dialer.Conn(dialCtx, address)
		},
		ForceAttemptHTTP2: false,
		TLSClientConfig:   websocketTLSConfig(p.config),
	}
	handshakeCtx, cancel := newTunnelHandshakeContext(ctx)
	defer cancel()
	connection, _, err := websocket.Dial(handshakeCtx, endpoint, &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: transport},
		Host:       p.config.WebSocketHost,
	})
	if err != nil {
		transport.CloseIdleConnections()
		return nil, fmt.Errorf("隧道 WebSocket 握手: %w", err)
	}
	return websocket.NetConn(context.Background(), connection, websocket.MessageBinary), nil
}

func (p *websocketProxy) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("WebSocket 隧道不支持 UDP")
}

type serverDialer struct {
	netapi.EmptyDispatch
	address string
}

func (d *serverDialer) Conn(ctx context.Context, _ netapi.Address) (net.Conn, error) {
	return newServerNetDialer().DialContext(ctx, "tcp", d.address)
}

func newServerNetDialer() *net.Dialer {
	return &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
}

func newTunnelHandshakeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, tunnelHandshakeTimeout)
}

func websocketTLSConfig(config Config) *tls.Config {
	tlsConfig := (&tlsProxy{config: config}).tlsConfig()
	tlsConfig.NextProtos = []string{"http/1.1"}
	return tlsConfig
}

func validateWebSocketHost(host string) error {
	parsed, err := url.Parse("http://" + strings.TrimSpace(host))
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("WebSocket host 无效")
	}
	if parsed.Port() != "" {
		if _, err := canonicalServer(parsed.Hostname(), parsed.Port()); err != nil {
			return errors.New("WebSocket host 无效")
		}
	}
	return nil
}

func (d *serverDialer) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("当前隧道代理不支持 UDP")
}

func canonicalServer(host, port string) (string, error) {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if host == "" {
		return "", errors.New("隧道代理服务器不能为空")
	}
	parsedPort, err := strconv.ParseUint(strings.TrimSpace(port), 10, 16)
	if err != nil || parsedPort == 0 {
		return "", errors.New("隧道代理端口无效")
	}
	return net.JoinHostPort(host, strconv.FormatUint(parsedPort, 10)), nil
}

func decodeBase64String(value string) (string, error) {
	decoded, err := decodeBase64Bytes(value)
	return string(decoded), err
}

func decodeBase64Bytes(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if decoded, err := encoding.DecodeString(value); err == nil {
			return decoded, nil
		}
	}
	return nil, errors.New("invalid base64")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func splitList(value string) []string {
	var result []string
	for item := range strings.SplitSeq(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}

func queryBool(query url.Values, names ...string) (bool, error) {
	for _, name := range names {
		if raw, ok := query[name]; ok && len(raw) != 0 {
			return parseBool(raw[0], name)
		}
	}
	return false, nil
}

func jsonBool(value map[string]any, name string) (bool, error) {
	raw, ok := value[name]
	if !ok || raw == nil {
		return false, nil
	}
	switch typed := raw.(type) {
	case bool:
		return typed, nil
	case string:
		return parseBool(typed, name)
	case float64:
		return parseBool(strconv.FormatFloat(typed, 'f', -1, 64), name)
	default:
		return false, fmt.Errorf("%s 必须是布尔值", name)
	}
}

func parseBool(value, name string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true, nil
	case "", "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s 必须是布尔值", name)
	}
}

func jsonString(value map[string]any, name string) string {
	raw, ok := value[name]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}

func jsonStringList(value map[string]any, name string) ([]string, error) {
	raw, ok := value[name]
	if !ok || raw == nil {
		return nil, nil
	}
	if text, ok := raw.(string); ok {
		return splitList(text), nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s 必须是字符串或字符串数组", name)
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s 必须是字符串或字符串数组", name)
		}
		if text = strings.TrimSpace(text); text != "" {
			result = append(result, text)
		}
	}
	return result, nil
}
