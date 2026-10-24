package weed_server

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/seaweedfs/seaweedfs/weed/glog"
	"github.com/seaweedfs/seaweedfs/weed/pb"
)

const (
	// licensePeerResolveTTL is how long resolved master host names are
	// reused, and how long a failed lookup is not repeated.
	licensePeerResolveTTL = 10 * time.Second
	// licensePeerResolveTimeout bounds one host name lookup. A few names
	// failing in a row must stay under the 3 second deadline of a
	// master-to-master RPC (license.peerRPCTimeout).
	licensePeerResolveTimeout = time.Second
	// licensePeerDenyLogInterval rate-limits the refusal log.
	licensePeerDenyLogInterval = time.Minute
)

// licensePeerGuard admits the RPCs that only masters send each other:
// GetLicense with include_state, GetLedger, SyncLicense and SyncLedger. The
// license state they carry includes internal_read_key, so the caller must
// connect from the address of a raft member, a -peers master, or an address
// in master.license.peer_ips (plan 1.3). It does not depend on gRPC TLS.
type licensePeerGuard struct {
	static []string     // hosts of this master and its -peers
	extra  []*net.IPNet // master.license.peer_ips

	mu          sync.Mutex
	resolved    map[string]resolvedHost
	lastDenyLog time.Time
	deniedSince int
}

type resolvedHost struct {
	ips []net.IP
	at  time.Time
}

func newLicensePeerGuard(self pb.ServerAddress, peers map[string]pb.ServerAddress, extra string) *licensePeerGuard {
	g := &licensePeerGuard{resolved: make(map[string]resolvedHost)}
	g.static = append(g.static, licensePeerHost(string(self)))
	for _, p := range peers {
		g.static = append(g.static, licensePeerHost(string(p)))
	}
	for _, s := range strings.Split(extra, ",") {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if ip := net.ParseIP(s); ip != nil {
			bits := 128
			if ip4 := ip.To4(); ip4 != nil {
				ip, bits = ip4, 32
			}
			g.extra = append(g.extra, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			glog.Fatalf("license: master.license.peer_ips: %q is not an IP or CIDR", s)
		}
		g.extra = append(g.extra, n)
	}
	return g
}

// licensePeerLookupIP resolves a master host name; tests replace it.
var licensePeerLookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// licensePeerHost returns the host of a master address or raft server id,
// which may carry a gRPC port as host:port.grpcPort.
func licensePeerHost(addr string) string {
	host, _, err := net.SplitHostPort(pb.ServerAddress(addr).ToHttpAddress())
	if err != nil {
		return addr
	}
	return host
}

// checkLicensePeer returns PermissionDenied unless the caller is a master
// of this cluster.
func (ms *MasterServer) checkLicensePeer(ctx context.Context) error {
	ip := licenseCallerIP(ctx)
	hosts := append([]string(nil), ms.licensePeers.static...)
	if m, err := (masterRaft{ms}).Membership(); err == nil {
		for _, id := range m.Voters {
			hosts = append(hosts, licensePeerHost(id))
		}
		for _, id := range m.Nonvoters {
			hosts = append(hosts, licensePeerHost(id))
		}
	}
	if ip != nil && ms.licensePeers.allows(ctx, ip, hosts) {
		return nil
	}
	ms.licensePeers.logDenied(ip, hosts)
	return status.Errorf(codes.PermissionDenied, "license: caller %v is not a master of this cluster", ip)
}

func licenseCallerIP(ctx context.Context) net.IP {
	pr, ok := peer.FromContext(ctx)
	if !ok || pr.Addr == nil {
		return nil
	}
	if a, ok := pr.Addr.(*net.TCPAddr); ok {
		return a.IP
	}
	host, _, err := net.SplitHostPort(pr.Addr.String())
	if err != nil {
		return nil
	}
	return net.ParseIP(host)
}

// allows reports whether ip is in master.license.peer_ips or is an address
// of one of hosts. IP literals are compared first; names are resolved only
// when no literal matches, each once (hosts lists -peers and raft members,
// which usually name the same masters).
func (g *licensePeerGuard) allows(ctx context.Context, ip net.IP, hosts []string) bool {
	for _, n := range g.extra {
		if n.Contains(ip) {
			return true
		}
	}
	var names []string
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		if hip := net.ParseIP(h); hip != nil {
			if hip.Equal(ip) {
				return true
			}
		} else if h != "" {
			names = append(names, h)
		}
	}
	for _, h := range names {
		for _, hip := range g.resolve(ctx, h) {
			if hip.Equal(ip) {
				return true
			}
		}
	}
	return false
}

// resolve looks up a master host name, reusing a recent result. A failed
// lookup keeps the previous result and is not repeated for the TTL either:
// without DNS every master-to-master RPC would otherwise wait for the
// lookup timeout per name and miss its deadline.
func (g *licensePeerGuard) resolve(ctx context.Context, host string) []net.IP {
	g.mu.Lock()
	r, ok := g.resolved[host]
	g.mu.Unlock()
	if ok && time.Since(r.at) < licensePeerResolveTTL {
		return r.ips
	}
	ctx, cancel := context.WithTimeout(ctx, licensePeerResolveTimeout)
	defer cancel()
	ips, err := licensePeerLookupIP(ctx, host)
	if err != nil {
		glog.V(1).Infof("license: resolve master %s: %v; keeping %v for %v", host, err, r.ips, licensePeerResolveTTL)
		ips = r.ips
	}
	g.mu.Lock()
	g.resolved[host] = resolvedHost{ips: ips, at: time.Now()}
	g.mu.Unlock()
	return ips
}

func (g *licensePeerGuard) logDenied(ip net.IP, hosts []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if time.Since(g.lastDenyLog) < licensePeerDenyLogInterval {
		g.deniedSince++
		return
	}
	seen := make(map[string]bool)
	var masters []string
	for _, h := range hosts {
		if !seen[h] {
			seen[h] = true
			masters = append(masters, h)
		}
	}
	more := ""
	if g.deniedSince > 0 {
		more = fmt.Sprintf(" (%d more refused in the last minute)", g.deniedSince)
	}
	glog.Warningf("license: refused a master-to-master license RPC from %v%s; master hosts: %s; add the address to master.license.peer_ips if a master connects from it (NAT, another interface)",
		ip, more, strings.Join(masters, ","))
	g.lastDenyLog = time.Now()
	g.deniedSince = 0
}
