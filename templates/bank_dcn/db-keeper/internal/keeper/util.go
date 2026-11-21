package keeper

import (
	"context"
	"net"
	"strings"
	"time"

	"github.com/hashicorp/raft"
)

func contextTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func nowISO() string { return time.Now().UTC().Format(time.RFC3339) }

// makeTransport builds the raft TCP transport for this node.
func makeTransport(addr string) *raft.NetworkTransport {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
		port = "18111"
	}
	adv, _ := net.ResolveTCPAddr("tcp", net.JoinHostPort(host, port))
	tr, err := raft.NewTCPTransport(net.JoinHostPort("0.0.0.0", port), adv, 3, 10*time.Second, nil)
	if err != nil {
		panic(err)
	}
	return tr
}

func stringsSplitTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
