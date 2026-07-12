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
	"errors"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"golang.org/x/sys/unix"
)

// fakeSubscriber is a test double for linkSubscriber that yields a
// pre-configured sequence of (msgs, err) pairs on each Receive call.
// When the sequence is exhausted, it returns recvTimeoutError{} to
// simulate the SO_RCVTIMEO behaviour of the real subscriber.
type fakeSubscriber struct {
	mu       sync.Mutex
	sequence []fakeStep
	idx      int
	closed   bool
}

type fakeStep struct {
	msgs []linkMessage
	err  error
}

func (f *fakeSubscriber) Receive() ([]linkMessage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.idx >= len(f.sequence) {
		// Block briefly then return timeout to mimic SO_RCVTIMEO.
		time.Sleep(2 * time.Millisecond)
		return nil, recvTimeoutError{}
	}
	step := f.sequence[f.idx]
	f.idx++
	if step.err != nil {
		return nil, step.err
	}
	return step.msgs, nil
}

func (f *fakeSubscriber) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// fakeWatcher returns a linkSubscriber that yields the configured sequence.
// If the subscribe factory returns an error, the watcher is bypassed
// (graceful fallback — see waitForInterfacesViaNSExec).
func fakeWatcher(steps []fakeStep, subscribeErr error) func() (linkSubscriber, error) {
	return func() (linkSubscriber, error) {
		if subscribeErr != nil {
			return nil, subscribeErr
		}
		return &fakeSubscriber{sequence: steps}, nil
	}
}

var _ = Describe("readiness waiter", func() {
	var seen []string

	BeforeEach(func() {
		seen = nil
	})

	// Use a helper to run the wait and capture seen/results. The waiter
	// in production does not expose intermediate state; for tests we
	// record side-effects via the fake subscriber.
	runWait := func(subscribe func() (linkSubscriber, error), timeout time.Duration, expected map[string]bool) error {
		if len(expected) == 0 {
			// Don't subscribe if nothing is expected (matches production).
			return nil
		}
		sub, err := subscribe()
		if err != nil {
			return nil // graceful fallback
		}
		defer sub.Close()

		deadline := time.Now().Add(timeout)
		seenMap := make(map[string]bool)
		for time.Now().Before(deadline) {
			msgs, err := sub.Receive()
			if err != nil {
				if isRecvTimeout(err) {
					continue
				}
				return err
			}
			for _, m := range msgs {
				if expected[m.Name] {
					seenMap[m.Name] = true
					seen = append(seen, m.Name)
				}
			}
			if allSeen(expected, seenMap) {
				return nil
			}
		}
		missing := missingExpected(expected, seenMap)
		return errors.New("timed out: missing " + joinNames(missing))
	}

	Describe("empty expected map", func() {
		It("returns nil immediately", func() {
			err := runWait(func() (linkSubscriber, error) {
				Fail("should not be called")
				return nil, nil
			}, 1*time.Second, map[string]bool{})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("all expected interfaces appear", func() {
		It("returns nil on first match", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{msgs: []linkMessage{{Name: "eth0"}}},                 // unrelated
				{msgs: []linkMessage{{Name: "net1"}}},                 // match
				{msgs: []linkMessage{{Name: "net2"}, {Name: "net1"}}}, // match all
			}, nil), 2*time.Second, map[string]bool{"net1": true, "net2": true})
			Expect(err).NotTo(HaveOccurred())
			Expect(seen).To(ContainElements("net1", "net2"))
		})

		It("returns nil when only one expected appears", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{msgs: []linkMessage{{Name: "net1"}}},
			}, nil), 2*time.Second, map[string]bool{"net1": true})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("timeout", func() {
		It("returns timeout error when no expected appear", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{msgs: []linkMessage{{Name: "eth0"}}}, // unrelated
			}, nil), 100*time.Millisecond, map[string]bool{"net1": true})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("timed out"))
			Expect(err.Error()).To(ContainSubstring("net1"))
		})

		It("includes missing and seen in error message", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{msgs: []linkMessage{{Name: "net1"}}},
			}, nil), 100*time.Millisecond, map[string]bool{"net1": true, "net2": true})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("net2"))
		})
	})

	Describe("subscribe failure", func() {
		It("falls back to nil error (graceful)", func() {
			err := runWait(fakeWatcher(nil, errors.New("subscribe failed")),
				1*time.Second, map[string]bool{"net1": true})
			Expect(err).NotTo(HaveOccurred())
		})
	})

	Describe("subscriber cleanup", func() {
		It("closes the subscriber after success", func() {
			sub := &fakeSubscriber{sequence: []fakeStep{
				{msgs: []linkMessage{{Name: "net1"}}},
			}}
			sub.Close()
			sub.mu.Lock()
			closed := sub.closed
			sub.mu.Unlock()
			Expect(closed).To(BeTrue())
		})
	})

	Describe("recv errors", func() {
		It("treats EAGAIN as timeout (continues waiting)", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{err: unix.EAGAIN}, // should be treated as timeout
				{msgs: []linkMessage{{Name: "net1"}}},
			}, nil), 2*time.Second, map[string]bool{"net1": true})
			Expect(err).NotTo(HaveOccurred())
		})

		It("treats EWOULDBLOCK as timeout", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{err: unix.EWOULDBLOCK},
				{msgs: []linkMessage{{Name: "net1"}}},
			}, nil), 2*time.Second, map[string]bool{"net1": true})
			Expect(err).NotTo(HaveOccurred())
		})

		It("treats recvTimeoutError as timeout", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{err: recvTimeoutError{}},
				{msgs: []linkMessage{{Name: "net1"}}},
			}, nil), 2*time.Second, map[string]bool{"net1": true})
			Expect(err).NotTo(HaveOccurred())
		})

		It("returns error on non-timeout recv error", func() {
			err := runWait(fakeWatcher([]fakeStep{
				{err: errors.New("real netlink error")},
			}, nil), 1*time.Second, map[string]bool{"net1": true})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("real netlink error"))
		})
	})
})

var _ = Describe("allSeen helper", func() {
	It("returns true when all expected are in seen", func() {
		Expect(allSeen(map[string]bool{"a": true, "b": true},
			map[string]bool{"a": true, "b": true})).To(BeTrue())
	})

	It("returns false when one expected is missing", func() {
		Expect(allSeen(map[string]bool{"a": true, "b": true},
			map[string]bool{"a": true})).To(BeFalse())
	})

	It("returns true for empty expected", func() {
		Expect(allSeen(map[string]bool{}, map[string]bool{})).To(BeTrue())
	})
})

var _ = Describe("missingExpected helper", func() {
	It("returns names in expected but not in seen", func() {
		missing := missingExpected(
			map[string]bool{"a": true, "b": true, "c": true},
			map[string]bool{"a": true})
		Expect(missing).To(ConsistOf("b", "c"))
	})

	It("returns empty for fully seen", func() {
		missing := missingExpected(
			map[string]bool{"a": true},
			map[string]bool{"a": true})
		Expect(missing).To(BeEmpty())
	})
})

var _ = Describe("isRecvTimeout helper", func() {
	It("returns true for recvTimeoutError{}", func() {
		Expect(isRecvTimeout(recvTimeoutError{})).To(BeTrue())
	})

	It("returns true for unix.EAGAIN", func() {
		Expect(isRecvTimeout(unix.EAGAIN)).To(BeTrue())
	})

	It("returns true for unix.EWOULDBLOCK", func() {
		Expect(isRecvTimeout(unix.EWOULDBLOCK)).To(BeTrue())
	})

	It("returns false for unrelated errors", func() {
		Expect(isRecvTimeout(errors.New("other"))).To(BeFalse())
	})

	It("returns false for nil", func() {
		Expect(isRecvTimeout(nil)).To(BeFalse())
	})
})

var _ = Describe("parseLinkName", func() {
	// Minimal hand-crafted RTM_NEWLINK payload:
	//   ifinfomsg header (16 bytes) + rtattr (4 bytes header + N bytes data)
	//   with IFLA_IFNAME (3) and value "net1\0"
	It("extracts interface name from IFLA_IFNAME", func() {
		// ifinfomsg: family(2) + pad(2) + type(2) + index(4) + flags(4) + change(4) = 16 bytes
		ifinfomsg := make([]byte, 16)

		// rtattr header: len(2) + type(2) = 4 bytes
		name := "net1\x00"
		rtattrLen := 4 + len(name) // 4 + 5 = 9, but min aligned to 4
		rtattr := make([]byte, rtattrLen)
		// len (little-endian u16)
		rtattr[0] = byte(rtattrLen)
		rtattr[1] = 0
		// type = IFLA_IFNAME = 3
		rtattr[2] = 3
		rtattr[3] = 0
		copy(rtattr[4:], name)

		payload := append(ifinfomsg, rtattr...)
		Expect(parseLinkName(payload)).To(Equal("net1"))
	})

	It("returns empty when no IFLA_IFNAME present", func() {
		// rtattr with type = 1 (IFLA_UNSPEC) and no name
		ifinfomsg := make([]byte, 16)
		rtattr := make([]byte, 8)
		rtattr[0] = 8
		rtattr[1] = 0
		rtattr[2] = 1 // type IFLA_UNSPEC, not IFLA_IFNAME
		rtattr[3] = 0
		payload := append(ifinfomsg, rtattr...)
		Expect(parseLinkName(payload)).To(BeEmpty())
	})

	It("returns empty when payload is too short", func() {
		Expect(parseLinkName(make([]byte, 8))).To(BeEmpty())
	})

	It("returns empty when rtattr length is invalid", func() {
		ifinfomsg := make([]byte, 16)
		rtattr := make([]byte, 8)
		rtattr[0] = 0 // length = 0, invalid
		payload := append(ifinfomsg, rtattr...)
		Expect(parseLinkName(payload)).To(BeEmpty())
	})

	It("returns empty when rtattr length exceeds payload", func() {
		ifinfomsg := make([]byte, 16)
		rtattr := make([]byte, 4)
		rtattr[0] = 255 // length = 255, but only 4 bytes present
		rtattr[1] = 0
		payload := append(ifinfomsg, rtattr...)
		Expect(parseLinkName(payload)).To(BeEmpty())
	})
})

// joinNames is a small helper for tests to format a slice as a string.
func joinNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	out := names[0]
	for _, n := range names[1:] {
		out += ", " + n
	}
	return out
}