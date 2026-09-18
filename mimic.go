package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"time"

	utls "github.com/refraction-networking/utls"
)

// BunHelloSpec replicates the ClientHello sniffed from opencode (Bun 1.3.14):
// exact suite order, extension set/order, signature algorithms, x25519
// keyshare and BoringSSL-style padding. Zen's free tier fingerprints it.
var BunHelloSpec = utls.ClientHelloSpec{
	CipherSuites: []uint16{
		0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030, 0xcca9, 0xcca8,
		0xc009, 0xc013, 0xc00a, 0xc014, 0x009c, 0x009d, 0x002f, 0x0035,
	},
	CompressionMethods: []byte{0},
	Extensions: []utls.TLSExtension{
		&utls.SNIExtension{},
		&utls.ExtendedMasterSecretExtension{},
		&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
		&utls.SupportedCurvesExtension{Curves: []utls.CurveID{utls.X25519, utls.CurveP256, utls.CurveP384}},
		&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
		&utls.SessionTicketExtension{},
		&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
		&utls.StatusRequestExtension{},
		&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
			utls.ECDSAWithP256AndSHA256,
			utls.PSSWithSHA256,
			utls.PKCS1WithSHA256,
			utls.ECDSAWithP384AndSHA384,
			utls.PSSWithSHA384,
			utls.PKCS1WithSHA384,
			utls.PSSWithSHA512,
			utls.PKCS1WithSHA512,
			utls.PKCS1WithSHA1,
		}},
		&utls.SCTExtension{},
		&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: utls.X25519}}},
		&utls.PSKKeyExchangeModesExtension{Modes: []uint8{1}},
		&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
		&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
	},
}

// MimicDial returns a dialer that hands out connections presenting the
// opencode/Bun TLS fingerprint. Drop-in for http.Transport.DialTLSContext.
func MimicDial(network, addr string, spec *utls.ClientHelloSpec) (net.Conn, error) {
	d := &net.Dialer{Timeout: 20 * time.Second}
	raw, err := d.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	uc := utls.UClient(raw, &utls.Config{ServerName: hostForSNI(addr)}, utls.HelloCustom)
	if err := uc.ApplyPreset(spec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("apply hello preset: %w", err)
	}
	if err := uc.Handshake(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return uc, nil
}

// MimicDialContext is the ctx-aware variant for http.Transport.DialTLSContext.
func MimicDialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 20 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	uc := utls.UClient(raw, &utls.Config{ServerName: hostForSNI(addr)}, utls.HelloCustom)
	if err := uc.ApplyPreset(&BunHelloSpec); err != nil {
		raw.Close()
		return nil, fmt.Errorf("apply hello preset: %w", err)
	}
	if err := uc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("utls handshake: %w", err)
	}
	return uc, nil
}

func hostForSNI(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	return host
}

const idAlphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// mintOpencodeID reproduces opencode's Identifier.create ascending scheme:
// prefix + "_" + hex(6 low bytes of ts<<12|counter) + 14 base62 chars.
// Zen's free tier gate validates this shape.
func mintOpencodeID(prefix string) string {
	now := (uint64(time.Now().UnixMilli()) << 12) | 1
	var tb [6]byte
	for i := 0; i < 6; i++ {
		tb[i] = byte(now >> (40 - 8*i))
	}
	return fmt.Sprintf("%s_%s%s", prefix, fmt.Sprintf("%x", tb[:]), randomBase62(14))
}

func randomBase62(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[b[i]%62]
	}
	return string(b)
}
