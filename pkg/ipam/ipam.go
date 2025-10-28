package ipam

import (
	"fmt"
	"net"
	"net/netip"
	"sort"
	"sync"

	"github.com/sirupsen/logrus"
)

// IPSubnet represents a single subnet managed by the IPAM. It holds the
// network configuration and the allocation status of all IPs within its range.
type IPSubnet struct {
	cidr      netip.Prefix
	start     net.IP
	end       net.IP
	broadcast net.IP
	ips       map[string]bool
}

// IPAllocator manages the allocation of IP addresses from one or more subnets.
// It is safe for concurrent use.
type IPAllocator struct {
	ipam  map[string]IPSubnet
	mutex sync.Mutex
}

// New creates and returns a new IPAllocator.
func New() *IPAllocator {
	ipam := make(map[string]IPSubnet)

	return &IPAllocator{
		ipam: ipam,
	}
}

// NewSubnet adds a new subnet to the IPAM. It validates that the start and end
// IPs are within the subnet and that the end IP is not the broadcast address.
// It pre-populates a map of all available IPs within the given range.
func (a *IPAllocator) NewSubnet(name string, subnet string, start string, end string) (err error) {
	s := IPSubnet{}
	s.start = net.ParseIP(start)
	s.end = net.ParseIP(end)

	ipnet, err := netip.ParsePrefix(subnet)
	if err != nil {
		return err
	}
	s.cidr = ipnet

	startIP, err := netip.ParseAddr(start)
	if err != nil {
		return err
	}
	startIPCheck := ipnet.Contains(startIP)
	if !startIPCheck {
		return fmt.Errorf("start address %s is not within subnet %s range", start, subnet)
	}

	endIP, err := netip.ParseAddr(end)
	if err != nil {
		return err
	}
	endIPCheck := ipnet.Contains(endIP)
	if !endIPCheck {
		return fmt.Errorf("end address %s is not within subnet %s range", end, subnet)
	}

	startAddr, _ := netip.AddrFromSlice(s.start)
	endAddr, _ := netip.AddrFromSlice(s.end)
	if startAddr.Compare(endAddr) > 0 {
		return fmt.Errorf("end address %s is smaller then the start address %s", end, start)
	}

	if ipnet.Addr().Is4() {
		subnetStart := net.IP(ipnet.Addr().AsSlice())
		subnetMask := net.CIDRMask(ipnet.Bits(), 32)
		subnetBroadcast := net.IP(make([]byte, 4))
		for i := range subnetStart {
			subnetBroadcast[i] = subnetStart[i] | ^subnetMask[i]
		}
		s.broadcast = subnetBroadcast

		if s.end.Equal(s.broadcast) {
			return fmt.Errorf("end address %s equals the broadcast address %s", s.end.String(), s.broadcast.String())
		}
	}

	// pre-allocate all ips between the start and end address
	allocatedIPs := make(map[string]bool)
	for ip := startAddr; endAddr.Compare(ip.Prev()) > 0; ip = ip.Next() {
		allocatedIPs[ip.Unmap().String()] = false
	}
	s.ips = allocatedIPs

	a.ipam[name] = s

	return
}

// DeleteSubnet removes a subnet from the IPAM.
func (a *IPAllocator) DeleteSubnet(name string) {
	delete(a.ipam, name)
}

// GetIP allocates an IP address from the specified subnet. If a specific IP is
// requested (givenIP is not empty), it attempts to allocate that IP. Otherwise,
// it allocates the next available IP from the pool.
func (a *IPAllocator) GetIP(name string, givenIP string) (string, error) {
	if _, exists := a.ipam[name]; !exists {
		return "", fmt.Errorf("network %s does not exists", name)
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	if givenIP != "" {
		gIP, err := netip.ParseAddr(givenIP)
		if err != nil {
			return "", err
		}
		gIPCheck := a.ipam[name].cidr.Contains(gIP)
		if !gIPCheck {
			return "", fmt.Errorf("given ip %s is not cidr %s", givenIP, a.ipam[name].cidr)
		}

		if a.ipam[name].broadcast != nil && a.ipam[name].broadcast.Equal(gIP.Unmap().AsSlice()) {
			return "", fmt.Errorf("given ip %s equals the broadcast address %s", givenIP, a.ipam[name].broadcast.String())
		}

		if allocated, ok := a.ipam[name].ips[givenIP]; ok {
			if allocated {
				return "", fmt.Errorf("given ip %s is already allocated", givenIP)
			}
			a.ipam[name].ips[givenIP] = true
			return givenIP, nil
		} else {
			return "", fmt.Errorf("given ip %s not found in network pool %s", givenIP, name)
		}
	}

	// Get all IPs from the map, parse them, and sort them
	keys := make([]netip.Addr, 0, len(a.ipam[name].ips))
	for k := range a.ipam[name].ips {
		ip, err := netip.ParseAddr(k)
		if err != nil {
			// This should ideally not happen as we validate on NewSubnet
			logrus.Errorf("failed to parse IP %s from IPAM map: %v", k, err)
			continue
		}
		keys = append(keys, ip)
	}

	sort.Slice(keys, func(i, j int) bool {
		return keys[i].Compare(keys[j]) < 0
	})

	// Iterate over the sorted IPs to find the first available one
	for _, ip := range keys {
		ipStr := ip.String()
		if !a.ipam[name].ips[ipStr] {
			a.ipam[name].ips[ipStr] = true
			return ipStr, nil
		}
	}

	return "", fmt.Errorf("no more ips left in network %s", name)
}

// ReleaseIP returns an IP address to the specified subnet's pool, making it
// available for future allocations.
func (a *IPAllocator) ReleaseIP(name string, givenIP string) (err error) {
	if _, exists := a.ipam[name]; !exists {
		return fmt.Errorf("network %s does not exists", name)
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	if givenIP == "" {
		return fmt.Errorf("given ip is empty")
	}

	gIP, err := netip.ParseAddr(givenIP)
	if err != nil {
		return err
	}
	gIPCheck := a.ipam[name].cidr.Contains(gIP)
	if !gIPCheck {
		return fmt.Errorf("given ip %s is not cidr %s", givenIP, a.ipam[name].cidr)
	}

	if allocated, ok := a.ipam[name].ips[givenIP]; ok {
		if allocated {
			a.ipam[name].ips[givenIP] = false
			return nil
		} else {
			return fmt.Errorf("given ip %s was not allocated", givenIP)
		}
	}

	return fmt.Errorf("given ip %s not found in network %s", givenIP, name)
}

// Used returns the number of allocated IPs in the specified subnet.
func (a *IPAllocator) Used(name string) (i int) {
	if _, exists := a.ipam[name]; !exists {
		logrus.Warnf("(ipam.Used) network %s does not exists", name)

		return
	}

	for _, allocated := range a.ipam[name].ips {
		if allocated {
			i++
		}
	}

	return
}

// Available returns the number of available (unallocated) IPs in the specified subnet.
func (a *IPAllocator) Available(name string) (i int) {
	if _, exists := a.ipam[name]; !exists {
		logrus.Warnf("(ipam.Available) network %s does not exists", name)

		return
	}

	for _, allocated := range a.ipam[name].ips {
		if !allocated {
			i++
		}
	}

	return
}

// Usage logs a summary of the IP allocation status for the specified subnet,
// including which IPs are currently allocated.
func (a *IPAllocator) Usage(name string) {
	if _, exists := a.ipam[name]; !exists {
		logrus.Warnf("(ipam.Usage) network %s does not exists", name)

		return
	}

	logrus.Infof("(ipam.Usage) %s: cidr=%s, start=%s, end=%s, broadcast=%s",
		name,
		a.ipam[name].cidr.String(),
		a.ipam[name].start.String(),
		a.ipam[name].end.String(),
		a.ipam[name].broadcast.String(),
	)

	var i int = 0
	logrus.Infof("(ipam.Usage) allocated ips:")
	for ip, allocated := range a.ipam[name].ips {
		if allocated {
			logrus.Infof("- %s", ip)
			i++
		}
	}

	logrus.Infof("(ipam.Usage) ipsinpool=%d, usedips=%d",
		len(a.ipam[name].ips),
		i,
	)
}
