package dbus

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Client is a sequential Unix-socket client. All operations, including auth and
// Hello, share the absolute deadline supplied to Dial. It is not safe for
// concurrent calls. Close may interrupt a blocked call.
type Client struct {
	conn     net.Conn
	reader   *bufio.Reader
	serial   uint32
	deadline time.Time
}

// Error is a remote D-Bus error reply. No remote error text is logged.
type Error struct{ Name string }

func (e *Error) Error() string { return "dbus: " + e.Name }

func unescapeAddress(value string) (string, error) {
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == '%' {
			i += 2
			if i >= len(value) {
				return "", errProtocol
			}
			continue
		}
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_/.*", rune(c))) {
			return "", errProtocol
		}
	}
	return url.PathUnescape(value)
}

func unixAddress(address string) (string, error) {
	if len(address) > 4096 {
		return "", errProtocol
	}
	for _, candidate := range strings.Split(address, ";") {
		if !strings.HasPrefix(candidate, "unix:") {
			continue
		}
		var path, abstract string
		seen := map[string]bool{}
		valid := true
		for _, kv := range strings.Split(candidate[5:], ",") {
			key, value, ok := strings.Cut(kv, "=")
			if !ok || seen[key] {
				valid = false
				break
			}
			seen[key] = true
			v, err := unescapeAddress(value)
			if err != nil || strings.ContainsRune(v, 0) {
				valid = false
				break
			}
			switch key {
			case "path":
				path = v
			case "abstract":
				abstract = v
			case "guid":
				if len(v) != 32 {
					valid = false
				} else {
					_, err = hex.DecodeString(v)
					valid = err == nil
				}
			default:
				valid = false
			}
			if !valid {
				break
			}
		}
		if !valid || path != "" && abstract != "" {
			continue
		}
		if strings.HasPrefix(path, "/") {
			return path, nil
		}
		if abstract != "" {
			return "\x00" + abstract, nil
		}
	}
	return "", errors.New("dbus: no supported unix address")
}

// Dial authenticates with EXTERNAL using the effective uid, then sends Hello.
// Only explicit Unix addresses are supported; it never autolaunches a bus.
func Dial(address string, deadline time.Time) (*Client, error) {
	if deadline.IsZero() || !time.Now().Before(deadline) {
		return nil, errors.New("dbus: expired or missing deadline")
	}
	path, err := unixAddress(address)
	if err != nil {
		return nil, err
	}
	conn, err := (&net.Dialer{Deadline: deadline}).Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return authenticate(conn, deadline)
}

func authenticate(conn net.Conn, deadline time.Time) (*Client, error) {
	var err error
	c := &Client{conn: conn, reader: bufio.NewReaderSize(conn, 1024), deadline: deadline}
	fail := func(err error) (*Client, error) { conn.Close(); return nil, err }
	if err = conn.SetDeadline(deadline); err != nil {
		return fail(err)
	}
	uid := hex.EncodeToString([]byte(strconv.Itoa(os.Geteuid())))
	if err = c.write([]byte("\x00AUTH EXTERNAL " + uid + "\r\n")); err != nil {
		return fail(err)
	}
	line, err := c.reader.ReadSlice('\n')
	if err != nil {
		return fail(err)
	}
	if len(line) != 37 || !strings.HasPrefix(string(line), "OK ") || !strings.HasSuffix(string(line), "\r\n") {
		return fail(errors.New("dbus: EXTERNAL rejected"))
	}
	if _, err = hex.DecodeString(string(line[3:35])); err != nil {
		return fail(errProtocol)
	}
	if err = c.write([]byte("BEGIN\r\n")); err != nil {
		return fail(err)
	}
	values, err := c.Call("org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", "Hello", "", "s")
	if err != nil {
		return fail(err)
	}
	name := values[0].(string)
	if !strings.HasPrefix(name, ":") || !validBusName(name) {
		return fail(errProtocol)
	}
	return c, nil
}
func (c *Client) Close() error { return c.conn.Close() }
func (c *Client) write(b []byte) error {
	for len(b) > 0 {
		n, err := c.conn.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// Call sends one method call and validates its reply serial and exact output
// signature. Unsolicited signals are ignored, with a bounded message count.
func (c *Client) Call(destination, path, iface, member, input, output string, args ...any) ([]any, error) {
	if !time.Now().Before(c.deadline) {
		return nil, errors.New("dbus: deadline exceeded")
	}
	if !supported(output) || c.serial == ^uint32(0) || !validBusName(destination) || !validInterface(iface) || !validMember(member) {
		return nil, errProtocol
	}
	c.serial++
	b, err := marshal(1, c.serial, []field{{1, "o", path}, {2, "s", iface}, {3, "s", member}, {6, "s", destination}}, input, args)
	if err != nil {
		return nil, err
	}
	if err = c.write(b); err != nil {
		c.conn.Close()
		return nil, err
	}
	for count := 0; count < 64; count++ {
		m, err := readMessage(c.reader)
		if err != nil {
			c.conn.Close()
			return nil, err
		}
		if m.kind == 4 {
			continue
		}
		if m.reply != c.serial || m.kind != 2 && m.kind != 3 {
			c.conn.Close()
			return nil, errProtocol
		}
		if m.kind == 3 {
			return nil, &Error{Name: m.errorName}
		}
		if m.signature != output {
			c.conn.Close()
			return nil, fmt.Errorf("%w: unexpected reply signature", errProtocol)
		}
		return m.values, nil
	}
	c.conn.Close()
	return nil, errProtocol
}
func (c *Client) Get(destination, path, iface, property string) (Variant, error) {
	v, err := c.Call(destination, path, "org.freedesktop.DBus.Properties", "Get", "ss", "v", iface, property)
	if err != nil {
		return Variant{}, err
	}
	return v[0].(Variant), nil
}
