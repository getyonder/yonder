package direct

import (
	"net"
	"testing"

	"golang.org/x/sys/unix"
)

// ownAddress gives an IPv4 address of this computer that is not the
// loopback, with the name of its interface.
func ownAddress(t *testing.T) (net.IP, string) {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				return ipn.IP, ifi.Name
			}
		}
	}
	t.Skip("this computer has no interface with an IPv4 address but the loopback")
	return nil, ""
}

func TestInterfaceOf(t *testing.T) {
	for name, addr := range map[string]*net.UDPAddr{
		"no address":                 nil,
		"no IP":                      {Port: 5000},
		"every address (IPv4)":       {IP: net.IPv4zero},
		"every address (IPv6)":       {IP: net.IPv6unspecified},
		"the loopback (IPv4)":        {IP: net.IPv4(127, 0, 0, 1)},
		"the loopback (IPv6)":        {IP: net.IPv6loopback},
		"an address of no interface": {IP: net.IPv4(192, 0, 2, 1)},
	} {
		if got := interfaceOf(addr); got != "" {
			t.Errorf("%s: interfaceOf = %q, want none", name, got)
		}
	}
	ip, want := ownAddress(t)
	if got := interfaceOf(&net.UDPAddr{IP: ip}); got != want {
		t.Errorf("interfaceOf(%s) = %q, want %q", ip, got, want)
	}
}

// boundTo gives the interface that a socket is tied to, "" for none.
func boundTo(t *testing.T, conn any) string {
	t.Helper()
	uc, ok := conn.(*net.UDPConn)
	if !ok {
		t.Fatalf("the socket is a %T", conn)
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var name string
	if cerr := raw.Control(func(fd uintptr) {
		name, err = unix.GetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE)
	}); cerr != nil || err != nil {
		t.Fatal(cerr, err)
	}
	return name
}

func TestOwnRouteTiesSocketToInterface(t *testing.T) {
	n, err := newOwnRoute()
	if err != nil || n == nil {
		t.Fatalf("newOwnRoute: %v, %v", n, err)
	}

	lo, err := n.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer lo.Close()
	if got := boundTo(t, lo); got != "" {
		t.Errorf("the loopback socket is tied to %q, want none", got)
	}

	ip, want := ownAddress(t)
	conn, err := n.ListenUDP("udp4", &net.UDPAddr{IP: ip})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if got := boundTo(t, conn); got != want {
		t.Errorf("the socket at %s is tied to %q, want %q", ip, got, want)
	}
	if got := conn.LocalAddr().(*net.UDPAddr); !got.IP.Equal(ip) || got.Port == 0 {
		t.Errorf("the socket is at %v, want %s and a port", got, ip)
	}
}
