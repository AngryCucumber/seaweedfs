package weed_server

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

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

// A master host name that stops resolving is looked up once per TTL, not
// on every RPC, and its last addresses stay valid meanwhile; a name that
// never resolved is refused without another lookup for the TTL. Names
// listed twice (-peers and raft members) are resolved once.
func TestLicensePeerGuardResolveFailure(t *testing.T) {
	defer func() {
		licensePeerLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}()
	var lookups atomic.Int32
	var fail atomic.Bool
	licensePeerLookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		lookups.Add(1)
		if fail.Load() {
			return nil, errors.New("dns unavailable")
		}
		if host == "master-1" {
			return []net.IP{net.ParseIP("10.0.0.1")}, nil
		}
		return nil, errors.New("no such host")
	}
	g := newLicensePeerGuard("master-1:9333", map[string]pb.ServerAddress{"master-1:9333": "master-1:9333"}, "")
	ctx := context.Background()
	hosts := []string{"master-1", "master-1", "master-2", "master-2"}

	// Both names are resolved once although listed twice.
	if !g.allows(ctx, net.ParseIP("10.0.0.1"), hosts) || lookups.Load() != 1 {
		t.Fatalf("first call: %d lookups", lookups.Load())
	}
	if g.allows(ctx, net.ParseIP("10.0.0.2"), hosts) || lookups.Load() != 2 {
		t.Fatalf("unknown caller: %d lookups", lookups.Load())
	}

	// DNS fails: the TTL has not expired, so nothing is looked up again.
	fail.Store(true)
	if !g.allows(ctx, net.ParseIP("10.0.0.1"), hosts) || g.allows(ctx, net.ParseIP("10.0.0.2"), hosts) || lookups.Load() != 2 {
		t.Fatalf("within the TTL: %d lookups", lookups.Load())
	}

	// The TTL expired: one failed lookup per name, the old addresses stay,
	// and the failure is not retried for another TTL.
	g.mu.Lock()
	for h, r := range g.resolved {
		r.at = r.at.Add(-licensePeerResolveTTL)
		g.resolved[h] = r
	}
	g.mu.Unlock()
	if !g.allows(ctx, net.ParseIP("10.0.0.1"), hosts) || lookups.Load() != 3 {
		t.Fatalf("after the TTL, allowed by the old address: %d lookups", lookups.Load())
	}
	if g.allows(ctx, net.ParseIP("10.0.0.2"), hosts) || lookups.Load() != 4 {
		t.Fatalf("after the TTL, refused: %d lookups", lookups.Load())
	}
	for i := 0; i < 10; i++ {
		if !g.allows(ctx, net.ParseIP("10.0.0.1"), hosts) || g.allows(ctx, net.ParseIP("10.0.0.2"), hosts) {
			t.Fatal("decision changed while DNS is down")
		}
	}
	if lookups.Load() != 4 {
		t.Fatalf("failed lookups repeated within the TTL: %d lookups", lookups.Load())
	}
	if r := g.resolved["master-1"]; time.Since(r.at) > time.Minute || len(r.ips) != 1 {
		t.Fatalf("failed lookup did not keep the old addresses with a fresh time: %+v", r)
	}
}
