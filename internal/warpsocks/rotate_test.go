package warpsocks

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"
)

// fakeExit is a minimal SOCKS5 exit: it echoes its own name back so a test can
// see which exit served a connection.
func fakeExit(t *testing.T, name string) (addr string, close func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				if _, err := socks5ServerHandshake(c); err != nil {
					return
				}
				if err := writeSocks5Reply(c, 0x00, nil); err != nil {
					return
				}
				_, _ = c.Write([]byte(name))
			}(c)
		}
	}()
	return ln.Addr().String(), func() { ln.Close() }
}

func dialAndRead(t *testing.T, pool *Pool) string {
	t.Helper()
	c, _, err := pool.Dial(context.Background(), "example.com:443")
	if err != nil {
		t.Fatalf("pool dial: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b, err := io.ReadAll(c)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// dialSocks5 consumes the CONNECT reply, so the exit name is all that is left.
	if len(b) != 3 {
		t.Fatalf("exit greeting = %q, want the 3-byte exit name", b)
	}
	return string(b)
}

func TestPoolRotatesPerConnection(t *testing.T) {
	a, ca := fakeExit(t, "AAA")
	defer ca()
	b, cb := fakeExit(t, "BBB")
	defer cb()

	pool, err := NewPool([]string{a, b})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	seen := map[string]int{}
	for i := 0; i < 4; i++ {
		seen[dialAndRead(t, pool)]++
	}
	if seen["AAA"] != 2 || seen["BBB"] != 2 {
		t.Fatalf("round-robin distribution = %v, want 2/2", seen)
	}
}

func TestPoolSkipsDeadExit(t *testing.T) {
	a, ca := fakeExit(t, "AAA")
	defer ca()
	dead, cd := fakeExit(t, "DDD")
	cd() // closed before use: dials fail

	pool, err := NewPool([]string{dead, a})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	for i := 0; i < 3; i++ {
		if got := dialAndRead(t, pool); got != "AAA" {
			t.Fatalf("exit used = %q, want the live one", got)
		}
	}
}

func TestServerServesRotatingExits(t *testing.T) {
	first, cf := fakeExit(t, "AAA")
	defer cf()
	second, cs := fakeExit(t, "BBB")
	defer cs()

	pool, err := NewPool([]string{first, second})
	if err != nil {
		t.Fatalf("new pool: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()

	srv := &Server{Addr: addr, Pool: pool}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(ctx) }()
	waitListening(t, addr)

	seen := map[string]int{}
	for i := 0; i < 2; i++ {
		conn := socks5Client(t, addr, "example.com:443")
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 3)
		if _, err := io.ReadFull(conn, buf); err != nil {
			t.Fatalf("read exit name: %v", err)
		}
		conn.Close()
		seen[string(buf)]++
	}
	if seen["AAA"] != 1 || seen["BBB"] != 1 {
		t.Fatalf("rotated exits = %v, want one connection each", seen)
	}
	cancel()
	if err := <-errCh; err != nil {
		t.Fatalf("server exit: %v", err)
	}
}

func TestParseExitList(t *testing.T) {
	got, err := ParseExitList("127.0.0.1:4001, socks5://127.0.0.1:4002")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"127.0.0.1:4001", "127.0.0.1:4002"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	if _, err := ParseExitList(""); err == nil {
		if _, perr := NewPool(nil); perr == nil {
			t.Fatal("empty pool accepted")
		}
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never accepted", addr)
}

// socks5Client performs the inbound handshake against the rotating front and
// returns the established tunnel.
func socks5Client(t *testing.T, proxyAddr, dest string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", proxyAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial front: %v", err)
	}
	if _, err := c.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("greeting: %v", err)
	}
	resp := make([]byte, 2)
	if _, err := io.ReadFull(c, resp); err != nil || resp[0] != 0x05 || resp[1] != 0x00 {
		t.Fatalf("greeting reply = %v err=%v", resp, err)
	}
	host, port, err := net.SplitHostPort(dest)
	if err != nil {
		t.Fatalf("split dest: %v", err)
	}
	req := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	req = append(req, host...)
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], mustPort(t, port))
	req = append(req, p[:]...)
	if _, err := c.Write(req); err != nil {
		t.Fatalf("connect request: %v", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(c, head); err != nil {
		t.Fatalf("connect reply: %v", err)
	}
	if head[1] != 0x00 {
		t.Fatalf("connect reply code = %d", head[1])
	}
	switch head[3] {
	case 0x01:
		_, _ = io.ReadFull(c, make([]byte, 4+2))
	case 0x04:
		_, _ = io.ReadFull(c, make([]byte, 16+2))
	case 0x03:
		l := make([]byte, 1)
		_, _ = io.ReadFull(c, l)
		_, _ = io.ReadFull(c, make([]byte, int(l[0])+2))
	}
	return c
}

func mustPort(t *testing.T, s string) uint16 {
	t.Helper()
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		t.Fatalf("port %q: %v", s, err)
	}
	return uint16(n)
}
