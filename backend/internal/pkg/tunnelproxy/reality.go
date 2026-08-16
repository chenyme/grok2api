package tunnelproxy

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Asutorufa/yuhaiin/pkg/net/netapi"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

// realityProxy follows the Xray Reality client handshake used by yuhaiin, but
// keeps the ClientHello selectable so Clash client-fingerprint values are not
// silently replaced with Chrome.
type realityProxy struct {
	netapi.EmptyDispatch
	dialer     netapi.Proxy
	serverName string
	publicKey  *ecdh.PublicKey
	shortID    [8]byte
	helloID    utls.ClientHelloID
	alpn       []string
}

func newRealityProxy(config Config, dialer netapi.Proxy) (netapi.Proxy, error) {
	publicKeyBytes, err := base64.RawURLEncoding.DecodeString(config.RealityPublicKey)
	if err != nil {
		return nil, fmt.Errorf("解析 Reality public key: %w", err)
	}
	publicKey, err := ecdh.X25519().NewPublicKey(publicKeyBytes)
	if err != nil {
		return nil, errors.New("Reality public key 无效")
	}
	var shortID [8]byte
	decodedLength, err := hex.Decode(shortID[:], []byte(config.RealityShortID))
	if err != nil || decodedLength > len(shortID) {
		return nil, errors.New("Reality short ID 无效")
	}
	helloID, ok := realityClientHelloID(config.ClientFingerprint)
	if !ok {
		return nil, fmt.Errorf("VLESS Reality 暂不支持客户端指纹 %q", config.ClientFingerprint)
	}
	return &realityProxy{
		dialer: dialer, serverName: config.ServerName, publicKey: publicKey,
		shortID: shortID, helloID: helloID, alpn: append([]string(nil), config.ALPN...),
	}, nil
}

func realityClientHelloID(fingerprint string) (utls.ClientHelloID, bool) {
	switch fingerprint {
	case "chrome":
		return utls.HelloChrome_Auto, true
	case "firefox":
		return utls.HelloFirefox_Auto, true
	case "safari":
		return utls.HelloSafari_Auto, true
	case "ios":
		return utls.HelloIOS_Auto, true
	case "edge":
		return utls.HelloEdge_Auto, true
	case "qq":
		return utls.HelloQQ_Auto, true
	default:
		return utls.ClientHelloID{}, false
	}
}

func (p *realityProxy) Conn(ctx context.Context, address netapi.Address) (net.Conn, error) {
	connection, err := p.dialer.Conn(ctx, address)
	if err != nil {
		return nil, err
	}
	handshakeCtx, cancel := newTunnelHandshakeContext(ctx)
	defer cancel()
	secure, err := p.handshake(handshakeCtx, connection)
	if err != nil {
		_ = connection.Close()
		return nil, fmt.Errorf("Reality 握手: %w", err)
	}
	return secure, nil
}

func (p *realityProxy) PacketConn(context.Context, netapi.Address) (net.PacketConn, error) {
	return nil, errors.New("Reality 隧道不支持 UDP")
}

func (p *realityProxy) handshake(ctx context.Context, connection net.Conn) (net.Conn, error) {
	verifier := &realityVerifier{serverName: p.serverName}
	secure, err := buildRealityClientHello(connection, p.serverName, p.alpn, p.helloID, verifier.verifyPeerCertificate)
	if err != nil {
		return nil, err
	}
	hello := secure.HandshakeState.Hello
	privateKey := secure.HandshakeState.State13.EcdheKey

	hello.SessionId = make([]byte, 32)
	copy(hello.Raw[39:], hello.SessionId)
	binary.BigEndian.PutUint64(hello.SessionId, uint64(time.Now().Unix()))
	hello.SessionId[0] = 1
	hello.SessionId[1] = 8
	hello.SessionId[2] = 1
	copy(hello.SessionId[8:], p.shortID[:])

	peerKey, err := privateKey.ECDH(p.publicKey)
	if err != nil {
		return nil, fmt.Errorf("Reality ECDH: %w", err)
	}
	verifier.authKey = peerKey
	if _, err := hkdf.New(sha256.New, peerKey, hello.Random[:20], []byte("REALITY")).Read(peerKey); err != nil {
		return nil, err
	}
	var aead cipher.AEAD
	if realityPrefersAESGCM(hello.CipherSuites) {
		block, cipherErr := aes.NewCipher(peerKey)
		if cipherErr != nil {
			return nil, cipherErr
		}
		aead, err = cipher.NewGCM(block)
	} else {
		aead, err = chacha20poly1305.New(peerKey)
	}
	if err != nil {
		return nil, err
	}
	aead.Seal(hello.SessionId[:0], hello.Random[20:], hello.SessionId[:16], hello.Raw)
	copy(hello.Raw[39:], hello.SessionId)

	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, err
	}
	if !verifier.verified {
		return nil, errors.New("Reality 身份验证失败")
	}
	return secure, nil
}

func buildRealityClientHello(connection net.Conn, serverName string, alpn []string, helloID utls.ClientHelloID, verify func([][]byte, [][]*x509.Certificate) error) (*utls.UConn, error) {
	config := &utls.Config{
		ServerName: serverName, InsecureSkipVerify: true, SessionTicketsDisabled: true,
		NextProtos: append([]string(nil), alpn...), VerifyPeerCertificate: verify,
	}
	secure := utls.UClient(connection, config, helloID)
	if err := secure.BuildHandshakeState(); err != nil {
		return nil, err
	}
	if secure.HandshakeState.Hello == nil || len(secure.HandshakeState.Hello.Raw) < 71 {
		return nil, errors.New("Reality ClientHello 无效")
	}
	if len(alpn) != 0 {
		patched := false
		for _, extension := range secure.Extensions {
			if alpnExtension, ok := extension.(*utls.ALPNExtension); ok {
				alpnExtension.AlpnProtocols = append([]string(nil), alpn...)
				patched = true
				break
			}
		}
		if !patched {
			secure.Extensions = append(secure.Extensions, &utls.ALPNExtension{AlpnProtocols: append([]string(nil), alpn...)})
		}
		config.NextProtos = append([]string(nil), alpn...)
		secure.HandshakeState.Hello.AlpnProtocols = append([]string(nil), alpn...)
	}
	privateKey := secure.HandshakeState.State13.EcdheKey
	if privateKey == nil || privateKey.Curve() != ecdh.X25519() {
		return nil, fmt.Errorf("Reality 客户端指纹 %q 未提供 X25519 TLS 1.3 key share", helloID.Client)
	}
	return secure, nil
}

func realityPrefersAESGCM(cipherSuites []uint16) bool {
	for _, cipherSuite := range cipherSuites {
		switch cipherSuite {
		case utls.TLS_AES_128_GCM_SHA256, utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256, utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384:
			return true
		case utls.TLS_CHACHA20_POLY1305_SHA256, utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256:
			return false
		}
	}
	return false
}

type realityVerifier struct {
	serverName string
	authKey    []byte
	verified   bool
}

func (v *realityVerifier) verifyPeerCertificate(rawCertificates [][]byte, _ [][]*x509.Certificate) error {
	certificates := make([]*x509.Certificate, 0, len(rawCertificates))
	for _, rawCertificate := range rawCertificates {
		certificate, err := x509.ParseCertificate(rawCertificate)
		if err != nil {
			return err
		}
		certificates = append(certificates, certificate)
	}
	if len(certificates) == 0 {
		return errors.New("Reality 服务端未返回证书")
	}
	if publicKey, ok := certificates[0].PublicKey.(ed25519.PublicKey); ok {
		digest := hmac.New(sha512.New, v.authKey)
		_, _ = digest.Write(publicKey)
		if bytes.Equal(digest.Sum(nil), certificates[0].Signature) {
			v.verified = true
			return nil
		}
	}
	options := x509.VerifyOptions{
		DNSName: v.serverName, Intermediates: x509.NewCertPool(), CurrentTime: time.Now(),
	}
	for _, certificate := range certificates[1:] {
		options.Intermediates.AddCert(certificate)
	}
	_, err := certificates[0].Verify(options)
	return err
}
