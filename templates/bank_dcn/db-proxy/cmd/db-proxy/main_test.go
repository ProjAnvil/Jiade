package main

import (
	"net"
	"testing"
)

func TestPutRoutingOpensListenerAndRoutes(t *testing.T) {
	// fake backend that just closes connections
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	p := New()
	if err := p.applyRouting([]setEntry{{
		Name: "t", Port: 23301, Nodes: []string{ln.Addr().String()}, Primary: ln.Addr().String(),
	}}); err != nil {
		t.Fatalf("applyRouting: %v", err)
	}
	c, err := net.Dial("tcp", "127.0.0.1:23301")
	if err != nil {
		t.Fatalf("dial proxy port: %v", err)
	}
	_ = c.Close()
}
