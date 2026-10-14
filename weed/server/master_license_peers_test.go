package weed_server

import (
	"context"
	"net"
	"testing"

	"google.golang.org/grpc/peer"

	"github.com/seaweedfs/seaweedfs/weed/pb"
)

func TestLicensePeerGuard(t *testing.T) {
	g := newLicensePeerGuard("10.0.0.1:9333", map[string]pb.ServerAddress{
		"10.0.0.2:9333.19333": "10.0.0.2:9333.19333",
		"localhost:9335":      "localhost:9335",
	}, "192.168.0.0/16, 172.16.0.9")
	ctx := context.Background()
	for _, c := range []struct {
		ip   string
		want bool
	}{
		{"10.0.0.1", true},        // this master
		{"10.0.0.2", true},        // -peers entry with a gRPC port
		{"::ffff:10.0.0.2", true}, // IPv4-mapped caller
		{"127.0.0.1", true},       // -peers host name
		{"192.168.3.4", true},     // peer_ips CIDR
		{"172.16.0.9", true},      // peer_ips address
		{"172.16.0.10", false},
		{"10.0.0.3", false},
	} {
		if got := g.allows(ctx, net.ParseIP(c.ip), g.static); got != c.want {
			t.Errorf("caller %s: allowed %v, want %v", c.ip, got, c.want)
		}
	}
	if !g.allows(ctx, net.ParseIP("10.0.0.3"), append(g.static, licensePeerHost("10.0.0.3:9333"))) {
		t.Error("a raft member must be allowed")
	}
}

func TestLicenseCallerIP(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{Addr: &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 50000}})
	if ip := licenseCallerIP(ctx); !ip.Equal(net.ParseIP("10.0.0.2")) {
		t.Fatalf("caller ip %v", ip)
	}
	if ip := licenseCallerIP(context.Background()); ip != nil {
		t.Fatalf("no peer: %v", ip)
	}
}
