//go:build linux

package wscedge

import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"time"
)

// icmpSummary sends count ICMP echo requests to an IPv4 address through the kernel's
// unprivileged ICMP socket (SOCK_DGRAM, no root and no ping binary) and returns a summary in
// ping's own format, so parsePingOutput reads it like the real thing. ok is false when this
// cannot be done here (IPv6 target, or the socket is not permitted).
//
// Why not exec ping: the relay's systemd unit filters @privileged system calls, which kills
// the ping binary (it calls capset) before it prints anything.
func icmpSummary(ip string, count int, interval, wait time.Duration) (summary string, ok bool) {
	dst := net.ParseIP(ip).To4()
	if dst == nil {
		return "", false
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, syscall.IPPROTO_ICMP)
	if err != nil {
		return "", false
	}
	defer syscall.Close(fd)
	var to syscall.Sockaddr = &syscall.SockaddrInet4{}
	copy(to.(*syscall.SockaddrInet4).Addr[:], dst)

	var rtts []time.Duration
	buf := make([]byte, 1500)
	for seq := 1; seq <= count; seq++ {
		msg := make([]byte, 16)
		msg[0], msg[1] = 8, 0 // echo request
		binary.BigEndian.PutUint16(msg[6:], uint16(seq))
		binary.BigEndian.PutUint16(msg[2:], icmpChecksum(msg)) // the kernel rewrites the id
		sent := time.Now()
		if err := syscall.Sendto(fd, msg, 0, to); err != nil {
			continue
		}
		deadline := sent.Add(wait)
		for {
			left := time.Until(deadline)
			if left <= 0 {
				break
			}
			tv := syscall.NsecToTimeval(left.Nanoseconds())
			syscall.SetsockoptTimeval(fd, syscall.SOL_SOCKET, syscall.SO_RCVTIMEO, &tv)
			n, _, err := syscall.Recvfrom(fd, buf, 0)
			if err != nil {
				break
			}
			// A datagram ICMP socket delivers the ICMP message without the IP header.
			if n >= 8 && buf[0] == 0 && binary.BigEndian.Uint16(buf[6:8]) == uint16(seq) {
				rtts = append(rtts, time.Since(sent))
				break
			}
		}
		if seq < count {
			time.Sleep(interval)
		}
	}

	loss := 100 * (count - len(rtts)) / count
	summary = fmt.Sprintf("%d packets transmitted, %d received, %d%% packet loss\n", count, len(rtts), loss)
	if len(rtts) > 0 {
		min, max, sum := rtts[0], rtts[0], time.Duration(0)
		for _, r := range rtts {
			sum += r
			if r < min {
				min = r
			}
			if r > max {
				max = r
			}
		}
		ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
		summary += fmt.Sprintf("rtt min/avg/max/mdev = %.3f/%.3f/%.3f/0.000 ms\n", ms(min), ms(sum/time.Duration(len(rtts))), ms(max))
	}
	return summary, true
}

// icmpChecksum is the standard Internet checksum (RFC 1071).
func icmpChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}
