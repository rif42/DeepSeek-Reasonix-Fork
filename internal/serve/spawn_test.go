package serve

import (
	"net"
	"sort"
	"testing"
	"time"
)

func TestParseSpawnBase(t *testing.T) {
	cases := []struct {
		in      string
		host    string
		port    int
		wantErr bool
	}{
		{"127.0.0.1:8787", "127.0.0.1", 8787, false},
		{"0.0.0.0:9000", "127.0.0.1", 9000, false},
		{"[::]:8788", "127.0.0.1", 8788, false},
		{"localhost:8787", "localhost", 8787, false},
		{"", "", 0, true},
		{"not-an-address", "", 0, true},
		{"127.0.0.1", "", 0, true},
	}
	for _, c := range cases {
		host, port, err := parseSpawnBase(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseSpawnBase(%q): want error, got %s:%d", c.in, host, port)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseSpawnBase(%q): %v", c.in, err)
			continue
		}
		if host != c.host || port != c.port {
			t.Errorf("parseSpawnBase(%q) = %s:%d, want %s:%d", c.in, host, port, c.host, c.port)
		}
	}
}

func TestFindFreePortSkipsOccupied(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	occupied := ln.Addr().(*net.TCPAddr).Port
	got, err := findFreePort("127.0.0.1", occupied, 20)
	if err != nil {
		t.Fatal(err)
	}
	if got == occupied {
		t.Fatalf("findFreePort returned the occupied port %d", got)
	}
}

func TestFindFreePortExhaustion(t *testing.T) {
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	ports := make([]int, 0, 4)
	for range 4 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		listeners = append(listeners, ln)
		ports = append(ports, ln.Addr().(*net.TCPAddr).Port)
	}
	sort.Ints(ports)
	// Every port in [ports[0], ports[3]] is occupied; scanning exactly that
	// range must exhaust.
	if _, err := findFreePort("127.0.0.1", ports[0], len(ports)); err == nil {
		t.Fatal("findFreePort: want exhaustion error with all candidates occupied")
	}
}

func TestChildSpecArgs(t *testing.T) {
	got := (childSpec{host: "127.0.0.1", port: 8788}).args()
	want := []string{"serve", "--addr", "127.0.0.1:8788"}
	if !equalStrings(got, want) {
		t.Fatalf("args without idle shutdown = %v, want %v", got, want)
	}
	got = (childSpec{host: "127.0.0.1", port: 8788, idleShutdown: 5 * time.Minute}).args()
	want = []string{"serve", "--addr", "127.0.0.1:8788", "--idle-shutdown", "5m0s"}
	if !equalStrings(got, want) {
		t.Fatalf("args with idle shutdown = %v, want %v", got, want)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
