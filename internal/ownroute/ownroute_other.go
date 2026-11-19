//go:build !linux

package ownroute

import (
	"errors"
	"net"
	"syscall"
)

// Only Linux has the tie: see the package's description.

// InterfaceOf gives no interface here.
func InterfaceOf(ip net.IP) string { return "" }

// InterfaceFor gives no interface here.
func InterfaceFor(host string) string { return "" }

// Local gives no interfaces here, so no tied listener is kept.
func Local() []Interface { return nil }

// Control gives no function here.
func Control(name string) func(network, address string, c syscall.RawConn) error { return nil }

// Share is not there on this system.
func Share(ln net.Listener) error { return errors.ErrUnsupported }

// Listen is not there on this system.
func Listen(name string, port int) (net.Listener, error) { return nil, errors.ErrUnsupported }
