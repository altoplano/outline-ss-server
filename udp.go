// Copyright 2018 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"errors"
	"log"
	"net"
	"time"

	"sync"

	"github.com/shadowsocks/go-shadowsocks2/shadowaead"
	"github.com/shadowsocks/go-shadowsocks2/socks"
)

type mode int

const (
	remoteServer mode = iota
	relayClient
	socksClient
)

const udpBufSize = 64 * 1024

// upack decripts src into dst. It tries each cipher until it finds one that authenticates
// correctly. dst and src must not overlap.
func unpack(dst, src []byte, ciphers map[string]shadowaead.Cipher) ([]byte, shadowaead.Cipher, error) {
	for id, cipher := range ciphers {
		log.Printf("Trying UDP cipher %v", id)
		buf, err := shadowaead.Unpack(dst, src, cipher)
		if err != nil {
			log.Printf("Failed UDP cipher %v: %v", id, err)
			continue
		}
		log.Printf("Selected UDP cipher %v", id)
		return buf, cipher, nil
	}
	return nil, nil, errors.New("could not find valid cipher")
}

// Listen on addr for encrypted packets and basically do UDP NAT.
func udpRemote(c net.PacketConn, ciphers map[string]shadowaead.Cipher) {
	defer c.Close()

	nm := newNATmap(config.UDPTimeout)
	cipherBuf := make([]byte, udpBufSize)
	textBuf := make([]byte, udpBufSize)

	for {
		func() {
			n, raddr, err := c.ReadFrom(cipherBuf)
			defer log.Printf("Done with %v", raddr.String())
			if err != nil {
				log.Printf("UDP remote read error: %v", err)
				return
			}
			log.Printf("Request from %v with %v bytes", raddr, n)
			buf, cipher, err := unpack(textBuf, cipherBuf[:n], ciphers)
			if err != nil {
				log.Printf("UDP remote unpack error: %v", err)
				return
			}

			tgtAddr := socks.SplitAddr(buf)
			if tgtAddr == nil {
				log.Printf("Failed to split target address from packet: %q", buf)
				return
			}

			tgtUDPAddr, err := net.ResolveUDPAddr("udp", tgtAddr.String())
			if err != nil {
				log.Printf("failed to resolve target UDP address: %v", err)
				return
			}

			payload := buf[len(tgtAddr):]

			pc := nm.Get(raddr.String())
			if pc == nil {
				pc, err = net.ListenPacket("udp", "")
				if err != nil {
					log.Printf("UDP remote listen error: %v", err)
					return
				}

				nm.Add(raddr, shadowaead.NewPacketConn(c, cipher), pc, remoteServer)
			}
			log.Printf("DEBUG UDP Nat: client %v <-> proxy exit %v", raddr, pc.LocalAddr())

			_, err = pc.WriteTo(payload, tgtUDPAddr) // accept only UDPAddr despite the signature
			if err != nil {
				log.Printf("UDP remote write error: %v", err)
				return
			}
		}()
	}
}

// Packet NAT table
type natmap struct {
	sync.RWMutex
	m       map[string]net.PacketConn
	timeout time.Duration
}

func newNATmap(timeout time.Duration) *natmap {
	m := &natmap{}
	m.m = make(map[string]net.PacketConn)
	m.timeout = timeout
	return m
}

func (m *natmap) Get(key string) net.PacketConn {
	m.RLock()
	defer m.RUnlock()
	return m.m[key]
}

func (m *natmap) Set(key string, pc net.PacketConn) {
	m.Lock()
	defer m.Unlock()

	m.m[key] = pc
}

func (m *natmap) Del(key string) net.PacketConn {
	m.Lock()
	defer m.Unlock()

	pc, ok := m.m[key]
	if ok {
		delete(m.m, key)
		return pc
	}
	return nil
}

func (m *natmap) Add(peer net.Addr, dst, src net.PacketConn, role mode) {
	m.Set(peer.String(), src)

	go func() {
		timedCopy(dst, peer, src, m.timeout, role)
		if pc := m.Del(peer.String()); pc != nil {
			pc.Close()
		}
	}()
}

// copy from src to dst at target with read timeout
func timedCopy(dst net.PacketConn, target net.Addr, src net.PacketConn, timeout time.Duration, role mode) error {
	buf := make([]byte, udpBufSize)

	for {
		src.SetReadDeadline(time.Now().Add(timeout))
		n, raddr, err := src.ReadFrom(buf)
		if err != nil {
			return err
		}

		switch role {
		case remoteServer: // server -> client: add original packet source
			srcAddr := socks.ParseAddr(raddr.String())
			log.Printf("DEBUG UDP response from %v to %v", srcAddr, target)
			copy(buf[len(srcAddr):], buf[:n])
			copy(buf, srcAddr)
			_, err = dst.WriteTo(buf[:len(srcAddr)+n], target)
		case relayClient: // client -> user: strip original packet source
			srcAddr := socks.SplitAddr(buf[:n])
			_, err = dst.WriteTo(buf[len(srcAddr):n], target)
		case socksClient: // client -> socks5 program: just set RSV and FRAG = 0
			_, err = dst.WriteTo(append([]byte{0, 0, 0}, buf[:n]...), target)
		}

		if err != nil {
			return err
		}
	}
}
