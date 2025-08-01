//go:build linux

package netutils

import (
	"bytes"
	"net/netip"
	"os"
	"slices"

	"github.com/moby/moby/v2/daemon/libnetwork/internal/netiputil"
	"github.com/moby/moby/v2/daemon/libnetwork/internal/resolvconf"
	"github.com/moby/moby/v2/daemon/libnetwork/nlwrap"
	"github.com/moby/moby/v2/daemon/libnetwork/ns"
	"github.com/moby/moby/v2/daemon/libnetwork/types"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
)

// InferReservedNetworks returns a list of network prefixes that seem to be
// used by the system and that would likely break it if they were assigned to
// some Docker networks. It uses two heuristics to build that list:
//
// 1. Nameservers configured in /etc/resolv.conf ;
// 2. On-link routes ;
//
// That 2nd heuristic was originally not limited to on-links -- all non-default
// routes were checked (see [1]). This proved to be not ideal at best and
// highly problematic at worst:
//
//   - VPN software and appliances doing split tunneling might push a small set
//     of routes for large, aggregated prefixes to avoid maintenance and
//     potential issues whenever a new subnet comes into use on internal
//     network. However, not all subnets from these aggregates might be in use.
//   - For full tunneling, especially when implemented with OpenVPN, the
//     situation is even worse as the host might end up with the two following
//     routes: 0.0.0.0/1 and 128.0.0.0/1. They are functionally
//     indistinguishable from a default route, yet the Engine was treating them
//     differently. With those routes, there was no way to use dynamic subnet
//     allocation at all. (see 'def1' on [2])
//   - A subnet covered by the default route can be used, or not. Same for
//     non-default and non-on-link routes. The type of route says little about
//     the availability of subnets it covers, except for on-link routes as they
//     specifically define what subnet the current host is part of.
//
// The 2nd heuristic was modified to be limited to on-link routes in PR #42598
// (first released in v23.0, see [3]).
//
// If these heuristics don't detect an overlap, users should change their daemon
// config to remove that overlapping prefix from `default-address-pools`. If a
// prefix is found to overlap but users care enough about it being associated
// to a Docker network they can still rely on static allocation.
//
// For IPv6, the 2nd heuristic isn't applied as there's no such thing as
// on-link routes for IPv6.
//
// [1]: https://github.com/moby/libnetwork/commit/56832d6d89bf0f9d5280849026ee25ae4ae5f22e
// [2]: https://community.openvpn.net/openvpn/wiki/Openvpn23ManPage
// [3]: https://github.com/moby/moby/pull/42598
func InferReservedNetworks(v6 bool) []netip.Prefix {
	var reserved []netip.Prefix

	// We don't really care if os.ReadFile fails here. It either doesn't exist,
	// or we can't read it for some reason.
	if rc, err := os.ReadFile(resolvconf.Path()); err == nil {
		reserved = slices.DeleteFunc(tryGetNameserversAsPrefix(rc), func(p netip.Prefix) bool {
			return p.Addr().Is6() != v6
		})
	}

	if !v6 {
		reserved = append(reserved, queryOnLinkRoutes()...)
	}

	slices.SortFunc(reserved, netiputil.PrefixCompare)
	return reserved
}

// tryGetNameserversAsPrefix returns nameservers (if any) listed in
// /etc/resolv.conf as CIDR blocks (e.g., "1.2.3.4/32"). It ignores
// failures to parse the file, as this utility is used as a "best-effort".
func tryGetNameserversAsPrefix(resolvConf []byte) []netip.Prefix {
	rc, err := resolvconf.Parse(bytes.NewBuffer(resolvConf), "")
	if err != nil {
		return nil
	}
	nsAddrs := rc.NameServers()
	nameservers := make([]netip.Prefix, 0, len(nsAddrs))
	for _, addr := range nsAddrs {
		nameservers = append(nameservers, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return nameservers
}

// queryOnLinkRoutes returns a list of on-link routes available on the host.
// Only IPv4 prefixes are returned as there's no such thing as on-link
// routes for IPv6.
func queryOnLinkRoutes() []netip.Prefix {
	routes, err := ns.NlHandle().RouteList(nil, netlink.FAMILY_V4)
	if err != nil {
		return nil
	}

	var prefixes []netip.Prefix
	for _, route := range routes {
		if route.Scope == netlink.SCOPE_LINK && route.Dst != nil && !route.Dst.IP.IsUnspecified() {
			if p, ok := netiputil.ToPrefix(route.Dst); ok {
				prefixes = append(prefixes, p)
			}
		}
	}

	return prefixes
}

// GenerateIfaceName returns an interface name using the passed in
// prefix and the length of random bytes. The api ensures that the
// there is no interface which exists with that name.
func GenerateIfaceName(nlh nlwrap.Handle, prefix string, length int) (string, error) {
	for i := 0; i < 3; i++ {
		name, err := GenerateRandomName(prefix, length)
		if err != nil {
			return "", err
		}
		if nlh.Handle == nil {
			_, err = nlwrap.LinkByName(name)
		} else {
			_, err = nlh.LinkByName(name)
		}
		if err != nil {
			if errors.As(err, &netlink.LinkNotFoundError{}) {
				return name, nil
			}
			return "", err
		}
	}
	return "", types.InternalErrorf("could not generate interface name")
}
