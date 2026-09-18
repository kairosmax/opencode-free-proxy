package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

const realIP = "172.65.90.23"

var caPEM, leafPEM, leafKey []byte

func init() {
	// CA
	cak, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "opencode-mitm-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &cak.PublicKey, cak)
	cacert, _ := x509.ParseCertificate(caDER)
	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	// leaf for opencode.ai
	lk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "opencode.ai"},
		DNSNames:     []string{"opencode.ai", "*.opencode.ai"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTpl, cacert, &lk.PublicKey, cak)
	leafPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	lb, _ := x509.MarshalECPrivateKey(lk)
	leafKey = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: lb})
}

func main() {
	os.WriteFile("/data/data/com.termux/files/usr/tmp/opencode/leaf.pem", leafPEM, 0600)
	os.WriteFile("/data/data/com.termux/files/usr/tmp/opencode/ca.pem", caPEM, 0600)
	os.WriteFile("/data/data/com.termux/files/usr/tmp/opencode/leaf.key", leafKey, 0600)

	cert, _ := tls.X509KeyPair(leafPEM, leafKey)

	ln, err := net.Listen("tcp", ":443")
	if err != nil {
		fmt.Println("listen:", err)
		return
	}
	fmt.Println("mitm ready on :443", time.Now().Format(time.RFC3339))

	for {
		raw, err := ln.Accept()
		if err != nil {
			continue
		}
		tc := newTapConn(raw)
		cc := tls.Server(tc, &tls.Config{Certificates: []tls.Certificate{cert}})
		go func(cc *tls.Conn, tc *tapConn) {
			defer cc.Close()
			var connBuf [4096]byte
			_ = cc.Handshake()
			if dumpErr := tc.dumpHello(); dumpErr == nil {
				// captured raw ClientHello bytes for fingerprint cloning
			}

			// read request
			br := newPrefixReader(cc, &connBuf)
			line, _ := br.readLine()
			if line == "" {
				return
			}
			fmt.Println("=== REQ", time.Now().Format(time.RFC3339))
			fmt.Println(line)
			hdrs := map[string]string{}
			var body, ctype string
			for {
				l, _ := br.readLine()
				if l == "" {
					break
				}
				parts := strings.SplitN(l, ":", 2)
				if len(parts) != 2 {
					continue
				}
				k := strings.TrimSpace(parts[0])
				v := strings.TrimSpace(parts[1])
				hdrs[strings.ToLower(k)] = v
				fmt.Println("  " + k + ": " + v)
			}
			if cl := hdrs["content-length"]; cl != "" && cl != "0" {
				n := 0
				fmt.Sscan(cl, &n)
				b := make([]byte, n)
				io.ReadFull(br, b)
				body = string(b)
			}
			if hdrs["content-type"] == "application/json" {
				var v any
				if json.Unmarshal([]byte(body), &v) == nil {
					b, _ := json.MarshalIndent(v, "", " ")
					body = string(b)
				}
			}
			ctype = hdrs["content-type"]
			if strings.HasPrefix(ctype, "application/json") {
				fmt.Println("  BODY:", body)
			}

			if strings.HasPrefix(ctype, "text/event-stream") || strings.HasPrefix(ctype, "text/plain") {
				fmt.Println("  [streaming/plain body: " + body + "]")
			}

			// tunnel to real server
			rs, err := tls.Dial("tcp", realIP+":443", &tls.Config{
				ServerName: "opencode.ai",
				RootCAs:    mustPool(),
			})
			if err != nil {
				fmt.Println("  upstream dial err:", err)
				return
			}
			defer rs.Close()

			// replay request with captured headers
			rq := line + "\r\n"
			for k, v := range hdrs {
				rq += k + ": " + v + "\r\n"
			}
			rq += "\r\n"
			io.WriteString(rs, rq)
			io.WriteString(rs, body)

			// relay upstream response back, log status line + headers only
			rr := newPrefixReader(rs, &connBuf)
			sline, _ := rr.readLine()
			fmt.Println("  <-", sline)
			var rc int
			fmt.Sscanf(sline, "HTTP/1.1 %d", &rc)
			for {
				l, _ := rr.readLine()
				if l == "" {
					break
				}
			}
			io.WriteString(cc, sline+"\r\n")
			for k, v := range hdrs {
				_ = k
				_ = v
			}
			io.WriteString(cc, "Content-Length: 0\r\nConnection: close\r\n\r\n")
			// just dump first chunk of upstream body for logging
			b := make([]byte, 4096)
			n, _ := rr.Read(b)
			if n > 0 {
				fmt.Printf("  body head (%d bytes): %s\n", n, string(b[:min(n, 600)]))
			}
			fmt.Println("  hex auth-ish:", hex.EncodeToString([]byte(hdrs["authorization"])), "len", len(hdrs["authorization"]))
		}(cc, tc)
	}
}

// tapConn tees the first bytes of the connection (the TLS ClientHello) into a
// capture buffer without disturbing the stream.
type tapConn struct {
	net.Conn
	mu  sync.Mutex
	buf []byte
	capN int
}

func newTapConn(c net.Conn) *tapConn {
	return &tapConn{Conn: c, capN: 2048}
}

func (t *tapConn) Read(b []byte) (int, error) {
	n, err := t.Conn.Read(b)
	if n > 0 {
		t.mu.Lock()
		need := t.capN - len(t.buf)
		if need > 0 {
			if need > n {
				need = n
			}
			t.buf = append(t.buf, b[:need]...)
		}
		t.mu.Unlock()
	}
	return n, err
}

// dumpHello parses the captured ClientHello and writes JA3 + full hex to disk.
func (t *tapConn) dumpHello() error {
	t.mu.Lock()
	h := append([]byte(nil), t.buf...)
	t.mu.Unlock()
	if len(h) < 5 || h[0] != 22 {
		return fmt.Errorf("not a TLS record")
	}
	if len(h) > 5 && len(h)-5 > int(h[3])<<8|int(h[4]) {
		// multiple records may have been coalesced; trim to first record
		recLen := int(h[3])<<8 | int(h[4])
		if recLen+5 <= len(h) {
			h = h[:recLen+5]
		}
	}
	os.WriteFile("/data/data/com.termux/files/usr/tmp/opencode/hello.bin", h, 0600)
	sni := extractSNI(h)
	ct++
	os.WriteFile(fmt.Sprintf("/data/data/com.termux/files/usr/tmp/opencode/hello_%03d_%s.bin", ct, strings.ReplaceAll(sni, ".", "-")), h, 0600)
	ja3 := computeJA3(h)
	fmt.Printf("=== HELLO sni=%q ja3=%s\n", sni, ja3)
	return nil
}

var ct int

func extractSNI(h []byte) string {
	p := 5
	p += 4
	p += 2 + 32
	sid := int(h[p])
	p++
	p += sid
	csLen := int(h[p])<<8 | int(h[p+1])
	p += 2 + csLen
	p += int(h[p]) + 1
	extLen := int(h[p])<<8 | int(h[p+1])
	p += 2
	end := p + extLen
	for p+4 <= end && p+4 <= len(h) {
		et := int(h[p])<<8 | int(h[p+1])
		el := int(h[p+2])<<8 | int(h[p+3])
		p += 4
		if et == 0 && el >= 7 {
			q := p + 5
			hn := int(h[p+3])<<8 | int(h[p+4])
			if p+5+hn <= len(h) {
				return string(h[q : q+hn])
			}
		}
		p += el
	}
	return "unknown"
}

func isGrease(v uint16) bool { return (v&0x0f0f) == 0x0a0a && ((v>>8)&0xff) == (v&0xff) }

func computeJA3(h []byte) string {
	// strip record + handshake headers
	p := 5
	if h[p] != 1 {
		return "bad-handshake-type"
	}
	hlen := int(h[p+1])<<16 | int(h[p+2])<<8 | int(h[p+3])
	p += 4
	end := p + hlen
	u16 := func(i int) uint16 { return uint16(h[i])<<8 | uint16(h[i+1]) }
	version := u16(p)
	p += 2 + 32
	sid := int(h[p])
	p++
	p += sid
	csLen := int(u16(p))
	p += 2
	suites := []string{}
	for i := 0; i < csLen; i += 2 {
		v := u16(p + i)
		if !isGrease(v) {
			suites = append(suites, fmt.Sprintf("%d", v))
		}
	}
	p += csLen
	p += int(h[p]) + 1
	extLen := int(u16(p))
	p += 2
	extEnd := p + extLen
	exts, curves, points := []string{}, []string{}, []string{}
	for p+4 <= extEnd && p+4 <= end {
		et := u16(p)
		el := int(u16(p + 2))
		p += 4
		if !isGrease(et) {
			exts = append(exts, fmt.Sprintf("%d", et))
		}
		if et == 10 && el >= 2 {
			gl := int(u16(p))
			for i := 0; i < gl && p+2+i+1 < len(h); i += 2 {
				g := u16(p + 2 + i)
				if !isGrease(g) {
					curves = append(curves, fmt.Sprintf("%d", g))
				}
			}
		}
		if et == 11 && el >= 1 {
			pl := int(h[p])
			for i := 0; i < pl && p+1+i < len(h); i++ {
				points = append(points, fmt.Sprintf("%d", h[p+1+i]))
			}
		}
		p += el
	}
	return fmt.Sprintf("%d,%s,%s,%s,%s",
		version,
		strings.Join(suites, "-"), strings.Join(exts, "-"),
		strings.Join(curves, "-"), strings.Join(points, "-"))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func mustPool() *x509.CertPool {
	pool := x509.NewCertPool()
	b, _ := os.ReadFile("/data/data/com.termux/files/usr/etc/tls/cert.pem")
	pool.AppendCertsFromPEM(b)
	return pool
}

// prefixReader lets us read a header then continue reading the rest.
type prefixReader struct {
	r   io.Reader
	buf *[4096]byte
	off int
	n   int
}

func newPrefixReader(r io.Reader, buf *[4096]byte) *prefixReader {
	return &prefixReader{r: r, buf: buf}
}

func (p *prefixReader) Read(b []byte) (int, error) {
	if p.off < p.n {
		n := copy(b, p.buf[p.off:p.n])
		p.off += n
		return n, nil
	}
	return p.r.Read(b)
}

func (p *prefixReader) readLine() (string, error) {
	var sb strings.Builder
	one := make([]byte, 1)
	for {
		n, err := p.Read(one)
		if n == 0 {
			if err != nil {
				return sb.String(), err
			}
			continue
		}
		if one[0] == '\n' {
			return strings.TrimRight(sb.String(), "\r"), nil
		}
		sb.WriteByte(one[0])
	}
}