package dbus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestUnixAddress(t *testing.T) {
	for _, tc := range []struct{ address, want string }{
		{"unix:path=/tmp/a%2Cb%25c", "/tmp/a,b%c"},
		{"tcp:host=invalid;unix:abstract=mybus", "\x00mybus"},
		{"unix:path=/tmp/bus,guid=0123456789abcdef0123456789abcdef", "/tmp/bus"},
	} {
		got, e := unixAddress(tc.address)
		if e != nil || got != tc.want {
			t.Fatalf("%q => %q (%v)", tc.address, got, e)
		}
	}
	for _, s := range []string{"autolaunch:", "unix:path=relative", "unix:path=/a,path=/b", "unix:path=/a,abstract=b", "unix:path=/a%00", "unix:path=/a%xx", "unix:path=/a@b", "unix:path=/a+b", "unix:path=/a b", "unix:path=/a,guid=x", "unix:tmpdir=/tmp", "unix:path=/a,unknown=x,guid=0123456789abcdef0123456789abcdef", strings.Repeat("a", 4097)} {
		if _, e := unixAddress(s); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func testServer(t *testing.T, handler func(net.Conn)) net.Conn {
	t.Helper()
	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		server.SetDeadline(time.Now().Add(2 * time.Second))
		handler(server)
	}()
	t.Cleanup(func() {
		client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("test server did not exit")
		}
	})
	return client
}

func authenticateServer(t *testing.T, conn net.Conn) *bufio.Reader {
	t.Helper()
	r := bufio.NewReader(conn)
	line, e := r.ReadString('\n')
	want := "\x00AUTH EXTERNAL " + hex.EncodeToString([]byte(strconv.Itoa(os.Geteuid()))) + "\r\n"
	if e != nil || line != want {
		t.Errorf("authentication %q (%v)", line, e)
		return nil
	}
	if _, e = conn.Write([]byte("OK 0123456789abcdef0123456789abcdef\r\n")); e != nil {
		t.Error(e)
		return nil
	}
	line, e = r.ReadString('\n')
	if e != nil || line != "BEGIN\r\n" {
		t.Errorf("BEGIN %q (%v)", line, e)
		return nil
	}
	return r
}
func TestReplySerialErrorsAndSignals(t *testing.T) {
	for _, mode := range []string{"correct", "wrong serial", "remote error", "signature mismatch", "signal flood"} {
		t.Run(mode, func(t *testing.T) {
			address := testServer(t, func(conn net.Conn) {
				r := authenticateServer(t, conn)
				if r == nil {
					return
				}
				m, e := readMessage(r)
				if e != nil {
					t.Error(e)
					return
				}
				if m.kind != 1 || m.serial != 1 {
					t.Errorf("Hello %+v", m)
					return
				}
				if mode == "signal flood" {
					b, _ := marshal(4, 2, []field{{1, "o", "/org/freedesktop/DBus"}, {2, "s", "org.freedesktop.DBus"}, {3, "s", "NameAcquired"}}, "s", []any{":1.1"})
					for i := 0; i < 64; i++ {
						if _, e = conn.Write(b); e != nil {
							return
						}
					}
					return
				}
				serial := uint32(1)
				if mode == "wrong serial" {
					serial = 9
				}
				if mode == "remote error" {
					conn.Write(reply(t, 3, 2, serial, "s", "not authorized"))
					return
				}
				if mode == "signature mismatch" {
					conn.Write(reply(t, 2, 2, serial, "u", uint32(3)))
					return
				}
				conn.Write(reply(t, 2, 2, serial, "s", ":1.1"))
			})
			c, e := authenticate(address, time.Now().Add(time.Second))
			if mode == "correct" {
				if e != nil {
					t.Fatal(e)
				}
				c.Close()
				return
			}
			if e == nil {
				c.Close()
				t.Fatal("accepted invalid reply")
			}
			if mode == "remote error" {
				var remote *Error
				if !errors.As(e, &remote) || remote.Name != "org.freedesktop.DBus.Error.AccessDenied" {
					t.Fatalf("remote error %v", e)
				}
			}
		})
	}
}
func TestAbsoluteDeadline(t *testing.T) {
	for _, phase := range []string{"auth", "hello", "call"} {
		t.Run(phase, func(t *testing.T) {
			address := testServer(t, func(conn net.Conn) {
				if phase == "auth" {
					bufio.NewReader(conn).ReadString('\n')
				} else {
					r := authenticateServer(t, conn)
					if r == nil {
						return
					}
					m, e := readMessage(r)
					if e != nil {
						t.Error(e)
						return
					}
					if phase == "call" {
						conn.Write(reply(t, 2, 2, m.serial, "s", ":1.1"))
						if _, e = readMessage(r); e != nil {
							t.Error(e)
							return
						}
					}
				}
				var b [1]byte
				conn.Read(b[:])
			})
			start := time.Now()
			c, e := authenticate(address, start.Add(80*time.Millisecond))
			if phase == "call" && e == nil {
				defer c.Close()
				_, e = c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId", "", "s")
			}
			if e == nil {
				t.Fatal("missing timeout")
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("unbounded %v", elapsed)
			}
		})
	}
	if _, e := Dial("unix:path=/invalid", time.Time{}); e == nil {
		t.Fatal("accepted missing deadline")
	}
}

func TestIsolatedDaemon(t *testing.T) {
	if os.Geteuid() < 0 {
		t.Skip("Unix EXTERNAL required")
	}
	daemon, e := exec.LookPath("dbus-daemon")
	if e != nil {
		t.Skip("dbus-daemon unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, daemon, "--session", "--nofork", "--nopidfile", "--print-address=1", "--address=unix:path="+filepath.Join(t.TempDir(), "bus"))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		t.Fatal(e)
	}
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	waited := false
	defer func() {
		cancel()
		if !waited {
			cmd.Wait()
		}
	}()
	address, e := bufio.NewReader(stdout).ReadString('\n')
	if e != nil {
		cancel()
		cmd.Wait()
		waited = true
		if strings.Contains(strings.ToLower(stderr.String()), "operation not permitted") {
			t.Skipf("sandbox prevents local dbus-daemon sockets: %s", strings.TrimSpace(stderr.String()))
		}
		t.Fatalf("daemon address: %v: %s", e, stderr.String())
	}
	c, e := Dial(strings.TrimSpace(address), time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	v, e := c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId", "", "s")
	if e != nil {
		t.Fatal(e)
	}
	id := v[0].(string)
	if len(id) != 32 {
		t.Fatalf("GetId %q", id)
	}
	if _, e = hex.DecodeString(id); e != nil {
		t.Fatal(e)
	}
	// Exercise Properties.Get's actual variant reply against the isolated daemon.
	vprop, e := c.Get("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "DoesNotExist")
	var remote *Error
	if !errors.As(e, &remote) {
		t.Fatalf("property %+v, want remote error, got %v", vprop, e)
	}
	_, e = c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "NoSuchMethod", "", "")
	if !errors.As(e, &remote) {
		t.Fatalf("method error %v", e)
	}
	// A remote error must not destroy the connection or poison serial matching.
	if _, e = c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId", "", "s"); e != nil {
		t.Fatal(e)
	}
}

func TestPropertiesAndConnectionAfterRemoteError(t *testing.T) {
	conn := testServer(t, func(conn net.Conn) {
		r := authenticateServer(t, conn)
		if r == nil {
			return
		}
		hello, e := readMessage(r)
		if e != nil {
			t.Error(e)
			return
		}
		conn.Write(reply(t, 2, 10, hello.serial, "s", ":1.42"))
		get, e := readMessage(r)
		if e != nil {
			t.Error(e)
			return
		}
		if get.serial != 2 || get.signature != "ss" || len(get.values) != 2 || get.values[0] != "org.freedesktop.RealtimeKit1" || get.values[1] != "RTTimeUSecMax" {
			t.Errorf("Get %+v", get)
			return
		}
		conn.Write(reply(t, 2, 11, get.serial, "v", Variant{"x", int64(200000)}))
		call, e := readMessage(r)
		if e != nil {
			t.Error(e)
			return
		}
		if call.serial != 3 {
			t.Error("nonmonotonic call serial")
			return
		}
		conn.Write(reply(t, 3, 12, call.serial, "s", "permission denied"))
		call, e = readMessage(r)
		if e != nil {
			t.Error(e)
			return
		}
		if call.serial != 4 {
			t.Error("remote error poisoned call serial")
			return
		}
		conn.Write(reply(t, 2, 13, call.serial, "s", "bus-id"))
	})
	c, e := authenticate(conn, time.Now().Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	v, e := c.Get("org.freedesktop.RealtimeKit1", "/org/freedesktop/RealtimeKit1", "org.freedesktop.RealtimeKit1", "RTTimeUSecMax")
	if e != nil || v.Signature != "x" || v.Value != int64(200000) {
		t.Fatalf("property %+v (%v)", v, e)
	}
	_, e = c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "NoSuchMethod", "", "")
	var remote *Error
	if !errors.As(e, &remote) {
		t.Fatalf("remote error %v", e)
	}
	values, e := c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "GetId", "", "s")
	if e != nil || values[0] != "bus-id" {
		t.Fatalf("connection after error %v (%v)", values, e)
	}
}

func TestHostileAuthentication(t *testing.T) {
	for _, line := range []string{"REJECTED EXTERNAL\r\n", "OK badguid\r\n", "OK zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz\r\n", strings.Repeat("a", 2048)} {
		t.Run(strconv.Itoa(len(line))+line[:2], func(t *testing.T) {
			conn := testServer(t, func(conn net.Conn) { bufio.NewReader(conn).ReadString('\n'); conn.Write([]byte(line)) })
			c, e := authenticate(conn, time.Now().Add(100*time.Millisecond))
			if e == nil {
				c.Close()
				t.Fatal("accepted hostile auth")
			}
		})
	}
}
func TestWriteDeadline(t *testing.T) {
	client, server := net.Pipe()
	defer server.Close()
	start := time.Now()
	c, e := authenticate(client, start.Add(60*time.Millisecond))
	if e == nil {
		c.Close()
		t.Fatal("unbounded write")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("write deadline not enforced: %v", elapsed)
	}
}
func TestNames(t *testing.T) {
	for _, s := range []string{"org.freedesktop.RealtimeKit1", "org._7.Valid"} {
		if !validInterface(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{"", "nodot", "org.7bad", "org..bad", "org.bad-name", strings.Repeat("a", 256) + ".b"} {
		if validInterface(s) {
			t.Fatalf("invalid interface accepted: %q", s)
		}
	}
	for _, s := range []string{":1.42", "org.freedesktop.DBus", "org.with-dash.Name"} {
		if !validBusName(s) {
			t.Fatal(s)
		}
	}
	for _, s := range []string{":", ":1", ".bad", "7.bad", ":1..2"} {
		if validBusName(s) {
			t.Fatalf("invalid bus accepted: %q", s)
		}
	}
	for _, s := range []string{"", "7Invalid", "Invalid.Member", "with-dash"} {
		if validMember(s) {
			t.Fatal(s)
		}
	}
}
