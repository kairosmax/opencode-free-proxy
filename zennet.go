package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	utls "github.com/refraction-networking/utls"
)

const zenClientHelloRecord = 512

func zenHelloSpec(host string) *utls.ClientHelloSpec {
	pad := 237 + len("opencode.ai") - len(host)
	if pad < 0 {
		pad = 0
	}
	return &utls.ClientHelloSpec{
		TLSVersMax:         utls.VersionTLS13,
		TLSVersMin:         utls.VersionTLS12,
		CompressionMethods: []byte{0},
		CipherSuites:       zenCipherSuites(),
		Extensions: []utls.TLSExtension{
			&utls.SNIExtension{ServerName: host},
			&utls.UtlsExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateNever},
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{29, 23, 24}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				0x0403, 0x0804, 0x0401, 0x0503, 0x0805, 0x0501, 0x0806, 0x0601, 0x0201,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{{Group: 29}}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{utls.VersionTLS13, utls.VersionTLS12}},
			&utls.GenericExtension{Id: 21, Data: make([]byte, pad)},
		},
	}
}

func zenCipherSuites() []uint16 {
	return []uint16{
		0x1301, 0x1302, 0x1303, 0xc02b, 0xc02f, 0xc02c, 0xc030,
		0xcca9, 0xcca8, 0xc009, 0xc013, 0xc00a, 0xc014,
		0x009c, 0x009d, 0x002f, 0x0035,
	}
}

func zenDialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	raw, err := d.DialContext(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	cfg := &utls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
	uconn := utls.UClient(raw, cfg, utls.HelloCustom)
	if err := uconn.ApplyPreset(zenHelloSpec(host)); err != nil {
		raw.Close()
		return nil, err
	}
	if err := uconn.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	return uconn, nil
}

var zenTransport = &http.Transport{
	DialTLSContext:      zenDialTLS,
	ForceAttemptHTTP2:   false,
	MaxIdleConns:        8,
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 20 * time.Second,
}

func zenHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Transport: zenTransport, Timeout: timeout}
}
