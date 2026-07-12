/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright 2026 Red Hat, Inc.
 *
 */

package netpod

import (
	"fmt"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// linkSubscriber abstracts a netlink subscription for testability.
type linkSubscriber interface {
	Receive() ([]linkMessage, error)
	Close()
}

// linkMessage is a minimal representation of a netlink RTM_NEWLINK
// message containing only the interface name.
type linkMessage struct {
	Name string
}

// netlinkLinkWatcher is the production implementation that subscribes
// to RTM_NEWLINK events in the current goroutine's netns.
type netlinkLinkWatcher struct{}

// Subscribe opens a netlink subscription in the current goroutine's netns.
// The caller must already be in the pod's netns (e.g., inside NSExec.Do).
func (netlinkLinkWatcher) Subscribe() (linkSubscriber, error) {
	conn, err := openLinkSubscriptionInCurrentNS()
	if err != nil {
		return nil, fmt.Errorf("failed to subscribe to netlink link events: %w", err)
	}
	return conn, nil
}

// readinessWaiter waits for a set of interface names to appear on the
// current netns via a netlink subscription. Returns nil if all appear
// before timeout, error otherwise.
type readinessWaiter struct {
	timeout time.Duration
}

// waitForInterfacesViaNSExec opens a netlink subscription in the current
// netns (caller is inside NSExec.Do) and waits for the expected interfaces
// to appear. Returns nil on subscribe failure (graceful fallback).
func (w readinessWaiter) waitForInterfacesViaNSExec(expected map[string]bool) error {
	if len(expected) == 0 {
		return nil
	}

	sub, err := netlinkLinkWatcher{}.Subscribe()
	if err != nil {
		// Don't block Setup() if we can't subscribe (e.g., restricted
		// container, missing privileges). The existing retry path via
		// the virt-handler workqueue will catch persistent failures.
		return nil
	}
	defer sub.Close()

	deadline := time.Now().Add(w.timeout)
	seen := make(map[string]bool)

	for time.Now().Before(deadline) {
		msgs, err := sub.Receive()
		if err != nil {
			if isRecvTimeout(err) {
				continue
			}
			return fmt.Errorf("netlink receive error: %w", err)
		}
		for _, m := range msgs {
			if expected[m.Name] {
				seen[m.Name] = true
			}
		}
		if allSeen(expected, seen) {
			return nil
		}
	}

	missing := missingExpected(expected, seen)
	return fmt.Errorf("timed out waiting for pod interfaces: missing %v (seen %v)", missing, seen)
}

func allSeen(expected, seen map[string]bool) bool {
	for name := range expected {
		if !seen[name] {
			return false
		}
	}
	return true
}

func missingExpected(expected, seen map[string]bool) []string {
	missing := make([]string, 0)
	for name := range expected {
		if !seen[name] {
			missing = append(missing, name)
		}
	}
	return missing
}

// isRecvTimeout reports whether the error from Receive() is a recv timeout.
// EAGAIN/EWOULDBLOCK are treated as timeouts because that's what the kernel
// returns when SO_RCVTIMEO expires with no messages.
func isRecvTimeout(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(recvTimeoutError); ok {
		return true
	}
	return err == unix.EAGAIN || err == unix.EWOULDBLOCK
}

// openLinkSubscriptionInCurrentNS opens a netlink subscription in whatever
// netns the current goroutine is running in. The caller is responsible for
// being in the correct netns.
func openLinkSubscriptionInCurrentNS() (linkSubscriber, error) {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW, unix.NETLINK_ROUTE)
	if err != nil {
		return nil, fmt.Errorf("socket: %w", err)
	}

	saddr := &unix.SockaddrNetlink{
		Family: unix.AF_NETLINK,
		Groups: 1 << (unix.RTNLGRP_LINK - 1),
	}
	if err := unix.Bind(fd, saddr); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("bind: %w", err)
	}

	// Set 500ms recv timeout so waitForInterfaces can loop until deadline.
	tv := unix.Timeval{Sec: 0, Usec: 500_000}
	if err := unix.SetsockoptTimeval(fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("set rcvtimeo: %w", err)
	}

	return &nlSubscriber{fd: fd}, nil
}

// nlSubscriber is the production linkSubscriber implementation backed
// by a raw AF_NETLINK socket.
type nlSubscriber struct {
	fd int
}

// Receive reads from the netlink socket, parses RTM_NEWLINK messages,
// and returns one linkMessage per interface name observed.
func (s *nlSubscriber) Receive() ([]linkMessage, error) {
	if s.fd < 0 {
		return nil, fmt.Errorf("subscriber closed")
	}

	buf := make([]byte, unix.SizeofNlMsghdr+64*1024)
	n, _, err := unix.Recvfrom(s.fd, buf, 0)
	if err != nil {
		if err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			return nil, recvTimeoutError{}
		}
		return nil, err
	}
	return parseLinkMessages(buf[:n]), nil
}

// Close closes the underlying socket.
func (s *nlSubscriber) Close() {
	if s.fd >= 0 {
		unix.Close(s.fd)
		s.fd = -1
	}
}

// recvTimeoutError signals that the netlink recv timed out with no messages.
type recvTimeoutError struct{}

func (recvTimeoutError) Error() string { return "netlink recv timeout" }

// parseLinkMessages walks a buffer of netlink messages and extracts
// interface names from RTM_NEWLINK messages.
func parseLinkMessages(buf []byte) []linkMessage {
	var out []linkMessage
	msgs, err := syscall.ParseNetlinkMessage(buf)
	if err != nil {
		return out
	}
	for _, m := range msgs {
		if m.Header.Type != unix.RTM_NEWLINK {
			continue
		}
		name := parseLinkName(m.Data)
		if name != "" {
			out = append(out, linkMessage{Name: name})
		}
	}
	return out
}

// parseLinkName decodes the IFLA_IFNAME attribute from an RTM_NEWLINK payload.
// Returns "" if no IFLA_IFNAME attribute is found.
func parseLinkName(data []byte) string {
	if len(data) < unix.SizeofIfInfomsg {
		return ""
	}
	attrs := data[unix.SizeofIfInfomsg:]
	for len(attrs) >= unix.SizeofRtAttr {
		// Manual alignment parse — avoid unsafe.Pointer use which may
		// cause issues on some Go versions with strict alignment.
		attrLen := int(uint16(attrs[4]) | uint16(attrs[5])<<8)
		attrType := int(uint16(attrs[2]) | uint16(attrs[3])<<8)

		if attrLen < unix.SizeofRtAttr || attrLen > len(attrs) {
			return ""
		}
		if attrType == unix.IFLA_IFNAME {
			nameStart := unix.SizeofRtAttr
			nameEnd := attrLen
			// null-terminate
			for i := nameStart; i < nameEnd; i++ {
				if attrs[i] == 0 {
					nameEnd = i
					break
				}
			}
			return string(attrs[nameStart:nameEnd])
		}
		// Align to 4 bytes
		aligned := (attrLen + 3) &^ 3
		if aligned <= 0 || aligned > len(attrs) {
			return ""
		}
		attrs = attrs[aligned:]
	}
	return ""
}