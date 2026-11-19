package ownroute

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// system reads this computer's interfaces that are up, without the
// loopback.
func system() []Interface {
	var out []Interface
	ifaces, _ := net.Interfaces()
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		ifc := Interface{Name: ifi.Name, Index: ifi.Index}
		addrs, _ := ifi.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok {
				ifc.Nets = append(ifc.Nets, n)
			}
		}
		out = append(out, ifc)
	}
	return out
}

// InterfaceOf gives the name of the interface that has the address, or ""
// for no address, the loopback, or an address of no interface.
func InterfaceOf(ip net.IP) string { return interfaceOf(system(), ip) }

// InterfaceFor gives the name of the interface whose network holds host,
// the IP address of another device, for a connection going out; "" when
// the connection is to be left to the system (see interfaceFor).
func InterfaceFor(host string) string { return interfaceFor(system(), host) }

// Local gives the interfaces that can lead to a device on a local
// network: one tied listener is kept for each (Listen).
func Local() []Interface { return local(system()) }

// Control gives the function for net.ListenConfig or net.Dialer that ties
// a socket to the named interface; nil for no name. A failure leaves the
// socket as the system makes it.
func Control(name string) func(network, address string, c syscall.RawConn) error {
	if name == "" {
		return nil
	}
	return func(_, _ string, c syscall.RawConn) error {
		c.Control(func(fd uintptr) { unix.BindToDevice(int(fd), name) })
		return nil
	}
}

// Share lets tied listeners (Listen) have the port of ln, a TCP listener
// for every interface. A plain listener that comes later is still refused
// the port, because it does not ask to share.
func Share(ln net.Listener) error {
	tcp, ok := ln.(*net.TCPListener)
	if !ok {
		return errors.ErrUnsupported
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return err
	}
	if cerr := raw.Control(func(fd uintptr) {
		err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
	}); cerr != nil {
		return cerr
	}
	return err
}

// Listen opens a TCP listener at port for IPv4 that is tied to the named
// interface, beside a listener for every interface that Share was called
// for. The system gives a connection that arrives on that interface to
// the tied listener, and its answers leave by that interface.
func Listen(name string, port int) (net.Listener, error) {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var err error
		if cerr := c.Control(func(fd uintptr) {
			if err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); err == nil {
				err = unix.BindToDevice(int(fd), name)
			}
		}); cerr != nil {
			return cerr
		}
		return err
	}}
	return lc.Listen(context.Background(), "tcp4", ":"+strconv.Itoa(port))
}
