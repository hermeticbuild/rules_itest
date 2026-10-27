package main

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"strconv"
	"syscall"
)

const maxPortAssignmentAttempts = 32

var (
	ipv4Loopback = netip.MustParseAddr("127.0.0.1")
	ipv6Loopback = netip.IPv6Loopback()
	loopbacks    = map[string][]netip.Addr{
		"ipv4":       {ipv4Loopback},
		"ipv6":       {ipv6Loopback},
		"dual-stack": {ipv4Loopback, ipv6Loopback},
	}
)

func portFamily(family string) string {
	if family == "" { // Specs produced before port_family default to IPv4.
		return "ipv4"
	}
	return family
}

func reserveServicePort(family, request string, reusable bool) ([]io.Closer, string, error) {
	addresses := loopbacks[portFamily(family)]
	if len(addresses) == 0 {
		return nil, "", fmt.Errorf("unknown port_family %q", family)
	}
	requested, err := strconv.ParseUint(request, 10, 16)
	if err != nil {
		return nil, "", fmt.Errorf("invalid port %q: %w", request, err)
	}

retry:
	for attempt := 1; ; attempt++ {
		port := uint16(requested)
		reservations := make([]io.Closer, 0, len(addresses))
		for i, address := range addresses {
			reservation, assigned, err := reservePortOnAddress(address, port, reusable)
			if err != nil {
				closeErr := closePorts(reservations)
				// Only bind errors remain bare when the failed socket closes successfully.
				bindErr, cleanBind := err.(syscall.Errno)
				if i > 0 && requested == 0 && closeErr == nil && attempt < maxPortAssignmentAttempts && cleanBind && isAddressInUse(bindErr) {
					continue retry
				}
				return nil, "", errors.Join(fmt.Errorf("reserve %s: %w", netip.AddrPortFrom(address, port), err), closeErr)
			}
			reservations = append(reservations, reservation)
			port = assigned
		}
		return reservations, strconv.Itoa(int(port)), nil
	}
}

func reservePortOnAddress(address netip.Addr, port uint16, reusable bool) (io.Closer, uint16, error) {
	if reusable && port != 0 {
		// A reusable bind can join an occupied port. Probe without sharing first;
		// a competing bind can still win the handoff to the reusable socket.
		probe, _, err := bindLoopbackPort(address, port, false)
		if err != nil {
			return nil, 0, err
		}
		if err := probe.Close(); err != nil {
			return nil, 0, fmt.Errorf("close availability probe: %w", err)
		}
	}
	return bindLoopbackPort(address, port, reusable)
}

func bindLoopbackPort(address netip.Addr, port uint16, reusable bool) (_ io.Closer, _ uint16, err error) {
	var family int
	var socketAddress syscall.Sockaddr
	if address.Is4() {
		family, socketAddress = syscall.AF_INET, &syscall.SockaddrInet4{Port: int(port), Addr: address.As4()}
	} else {
		family, socketAddress = syscall.AF_INET6, &syscall.SockaddrInet6{Port: int(port), Addr: address.As16()}
	}

	fd, socket, err := openPortSocket(family)
	if err != nil {
		return nil, 0, fmt.Errorf("socket: %w", err)
	}
	defer func() {
		if err == nil {
			return
		}
		if closeErr := socket.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close socket: %w", closeErr))
		}
	}()
	if reusable {
		if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, reusablePortOption, 1); err != nil {
			return nil, 0, fmt.Errorf("set reusable socket option: %w", err)
		}
	}
	if err := syscall.Bind(fd, socketAddress); err != nil {
		return nil, 0, err
	}
	addressResult, err := syscall.Getsockname(fd)
	if err != nil {
		return nil, 0, fmt.Errorf("getsockname: %w", err)
	}
	switch address := addressResult.(type) {
	case *syscall.SockaddrInet4:
		return socket, uint16(address.Port), nil
	case *syscall.SockaddrInet6:
		return socket, uint16(address.Port), nil
	default:
		return nil, 0, fmt.Errorf("getsockname returned %T, expected an IP socket address", address)
	}
}
