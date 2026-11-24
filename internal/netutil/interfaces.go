package netutil

import (
	"net"
	"sort"
	"strings"
)

// NetInterface represents an active local IPv4 network interface suitable for LAN broadcasting.
type NetInterface struct {
	Name   string     `json:"name"`
	IP     string     `json:"ip"`
	CIDR   string     `json:"cidr"`
	Subnet *net.IPNet `json:"-"`
}

// ListPrivateIPv4 returns active private IPv4 interfaces sorted with physical Wi-Fi/Ethernet first.
func ListPrivateIPv4() ([]NetInterface, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var out []NetInterface
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if isVirtualOrTunnelIface(iface.Name) {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLinkLocalUnicast() || !isPrivateOrCGNAT(ip4) {
				continue
			}
			masked := &net.IPNet{
				IP:   ip4.Mask(ipNet.Mask),
				Mask: ipNet.Mask,
			}
			out = append(out, NetInterface{
				Name:   iface.Name,
				IP:     ip4.String(),
				CIDR:   masked.String(),
				Subnet: masked,
			})
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		pi := ifacePriority(out[i].Name)
		pj := ifacePriority(out[j].Name)
		if pi != pj {
			return pi < pj
		}
		return out[i].Name < out[j].Name
	})

	return out, nil
}

// PickDefaultInterface selects the preferred LAN IPv4 interface or falls back to loopback.
func PickDefaultInterface(preferredName string) (NetInterface, []NetInterface, error) {
	all, err := ListPrivateIPv4()
	if err != nil {
		return loopbackInterface(), nil, err
	}
	if len(all) == 0 {
		return loopbackInterface(), all, nil
	}
	if preferredName != "" {
		for _, iface := range all {
			if iface.Name == preferredName {
				return iface, all, nil
			}
		}
	}
	return all[0], all, nil
}

// IsLoopbackRemote checks whether an HTTP RemoteAddr belongs to localhost.
func IsLoopbackRemote(remoteAddr string) bool {
	ip := parseRemoteIP(remoteAddr)
	return ip != nil && ip.IsLoopback()
}

// IsSameSubnetOrLoopback verifies that a client address belongs to the chosen LAN subnet or loopback.
func IsSameSubnetOrLoopback(remoteAddr string, subnet *net.IPNet) bool {
	ip := parseRemoteIP(remoteAddr)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	if subnet == nil {
		return isPrivateOrCGNAT(ip)
	}
	return subnet.Contains(ip)
}

func loopbackInterface() NetInterface {
	_, loNet, _ := net.ParseCIDR("127.0.0.0/8")
	return NetInterface{
		Name:   "lo0",
		IP:     "127.0.0.1",
		CIDR:   "127.0.0.0/8",
		Subnet: loNet,
	}
}

func parseRemoteIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	// Strip IPv6 zone if present.
	if idx := strings.IndexByte(host, '%'); idx >= 0 {
		host = host[:idx]
	}
	return net.ParseIP(host)
}

func isPrivateOrCGNAT(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4.IsPrivate() {
		return true
	}
	// 100.64.0.0/10 (Tailscale / carrier NAT)
	return ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127
}

func isVirtualOrTunnelIface(name string) bool {
	lower := strings.ToLower(name)
	for _, prefix := range []string{"utun", "awdl", "llw", "anpi", "bridge", "gif", "stf", "ap", "docker", "veth"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

func ifacePriority(name string) int {
	if name == "en0" {
		return 0
	}
	if name == "en1" {
		return 1
	}
	if strings.HasPrefix(name, "en") {
		return 2
	}
	return 10
}
