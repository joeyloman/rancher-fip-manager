package ipam

import (
	"fmt"
	"testing"
)

func TestIpam(t *testing.T) {
	ti := New()

	testSubnets := []struct {
		name   string
		subnet string
		start  string
		end    string
		want   error
	}{
		{
			name:   "default/network-class-c-ok",
			subnet: "192.168.0.0/24",
			start:  "192.168.0.10",
			end:    "192.168.0.254",
			want:   nil,
		},
		{
			name:   "default/network-class-c-start-error",
			subnet: "192.168.0.0/24",
			start:  "192.168.1.10",
			end:    "192.168.0.254",
			want:   fmt.Errorf("start address 192.168.1.10 is not within subnet 192.168.0.0/24 range"),
		},
		{
			name:   "default/network-class-c-end-error",
			subnet: "192.168.0.0/24",
			start:  "192.168.0.10",
			end:    "192.168.1.125",
			want:   fmt.Errorf("end address 192.168.1.125 is not within subnet 192.168.0.0/24 range"),
		},
		{
			name:   "default/network-class-c-smaller-error",
			subnet: "192.168.0.0/24",
			start:  "192.168.0.200",
			end:    "192.168.0.100",
			want:   fmt.Errorf("end address 192.168.0.100 is smaller then the start address 192.168.0.200"),
		},
		{
			name:   "default/network-class-c-broadcast-error",
			subnet: "192.168.0.0/24",
			start:  "192.168.0.10",
			end:    "192.168.0.255",
			want:   fmt.Errorf("end address 192.168.0.255 equals the broadcast address 192.168.0.255"),
		},
		{
			name:   "default/network-class-b-ok",
			subnet: "172.16.0.0/16",
			start:  "172.16.0.10",
			end:    "172.16.255.254",
			want:   nil,
		},
		{
			name:   "default/network-class-b-start-error",
			subnet: "172.16.0.0/16",
			start:  "172.10.0.10",
			end:    "172.16.254.254",
			want:   fmt.Errorf("start address 172.10.0.10 is not within subnet 172.16.0.0/16 range"),
		},
		{
			name:   "default/network-class-b-end-error",
			subnet: "172.16.0.0/16",
			start:  "172.16.0.10",
			end:    "172.200.1.125",
			want:   fmt.Errorf("end address 172.200.1.125 is not within subnet 172.16.0.0/16 range"),
		},
		{
			name:   "default/network-class-b-smaller-error",
			subnet: "172.16.0.0/16",
			start:  "172.16.180.10",
			end:    "172.16.0.100",
			want:   fmt.Errorf("end address 172.16.0.100 is smaller then the start address 172.16.180.10"),
		},
		{
			name:   "default/network-class-b-broadcast-error",
			subnet: "172.16.0.0/16",
			start:  "172.16.0.10",
			end:    "172.16.255.255",
			want:   fmt.Errorf("end address 172.16.255.255 equals the broadcast address 172.16.255.255"),
		},
		{
			name:   "default/network-class-a-ok",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.10",
			end:    "10.255.255.254",
			want:   nil,
		},
		{
			name:   "default/network-class-a-start-error",
			subnet: "10.0.0.0/8",
			start:  "11.0.0.10",
			end:    "10.255.255.254",
			want:   fmt.Errorf("start address 11.0.0.10 is not within subnet 10.0.0.0/8 range"),
		},
		{
			name:   "default/network-class-a-end-error",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.10",
			end:    "250.255.255.254",
			want:   fmt.Errorf("end address 250.255.255.254 is not within subnet 10.0.0.0/8 range"),
		},
		{
			name:   "default/network-class-a-smaller-error",
			subnet: "10.0.0.0/8",
			start:  "10.255.255.253",
			end:    "10.10.227.10",
			want:   fmt.Errorf("end address 10.10.227.10 is smaller then the start address 10.255.255.253"),
		},
		{
			name:   "default/network-class-a-broadcast-error",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.10",
			end:    "10.255.255.255",
			want:   fmt.Errorf("end address 10.255.255.255 equals the broadcast address 10.255.255.255"),
		},
		{
			name:   "default/network-class-c-small",
			subnet: "192.168.10.64/31",
			start:  "192.168.10.64",
			end:    "192.168.10.64",
			want:   nil,
		},
	}

	// NewSubnet function tests
	for i := 0; i < len(testSubnets); i++ {
		if got := ti.NewSubnet(
			testSubnets[i].name,
			testSubnets[i].subnet,
			testSubnets[i].start,
			testSubnets[i].end,
		); got != testSubnets[i].want {
			if got == nil || testSubnets[i].want == nil {
				t.Errorf("got %q, wanted %q", got, testSubnets[i].want)
			} else if got.Error() != testSubnets[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, testSubnets[i].want)
			}
		}
	}

	allocateIPs := []struct {
		subnetName string
		ip         string
		want       error
	}{
		{
			subnetName: "default/not-existing-network-class",
			ip:         "",
			want:       fmt.Errorf("network default/not-existing-network-class does not exists"),
		},
		{
			subnetName: "default/network-class-c-ok",
			ip:         "192.168.0.58",
			want:       nil,
		},
		{
			subnetName: "default/network-class-c-ok",
			ip:         "192.168.1.190",
			want:       fmt.Errorf("given ip 192.168.1.190 is not cidr 192.168.0.0/24"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.11",
			want:       nil,
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.255.255",
			want:       fmt.Errorf("given ip 172.16.255.255 equals the broadcast address 172.16.255.255"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.11",
			want:       fmt.Errorf("given ip 172.16.0.11 is already allocated"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.10",
			want:       nil,
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "",
			want:       nil,
		},
		{
			subnetName: "default/network-class-c-small",
			ip:         "",
			want:       nil,
		},
		{
			subnetName: "default/network-class-c-small",
			ip:         "",
			want:       fmt.Errorf("no more ips left in network default/network-class-c-small"),
		},
	}

	// GetIP function tests
	for i := 0; i < len(allocateIPs); i++ {
		_, got := ti.GetIP(
			allocateIPs[i].subnetName,
			allocateIPs[i].ip,
		)
		if got != allocateIPs[i].want {
			if got == nil || allocateIPs[i].want == nil {
				t.Errorf("got %q, wanted %q", got, allocateIPs[i].want)
			} else if got.Error() != allocateIPs[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, allocateIPs[i].want)
			}
		}
	}

	releaseIPs := []struct {
		subnetName string
		ip         string
		want       error
	}{
		{
			subnetName: "default/not-existing-network-class",
			ip:         "",
			want:       fmt.Errorf("network default/not-existing-network-class does not exists"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.11",
			want:       nil,
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "",
			want:       fmt.Errorf("given ip is empty"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.18.128.129",
			want:       fmt.Errorf("given ip 172.18.128.129 is not cidr 172.16.0.0/16"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.11",
			want:       fmt.Errorf("given ip 172.16.0.11 was not allocated"),
		},
		{
			subnetName: "default/network-class-b-ok",
			ip:         "172.16.0.5",
			want:       fmt.Errorf("given ip 172.16.0.5 not found in network default/network-class-b-ok"),
		},
	}

	// ReleaseIP function tests
	for i := 0; i < len(releaseIPs); i++ {
		if got := ti.ReleaseIP(
			releaseIPs[i].subnetName,
			releaseIPs[i].ip,
		); got != releaseIPs[i].want {
			if got == nil || releaseIPs[i].want == nil {
				t.Errorf("got %q, wanted %q", got, releaseIPs[i].want)
			} else if got.Error() != releaseIPs[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, releaseIPs[i].want)
			}
		}
	}

	// Used and Available funtion tests
	used := ti.Used("default/network-class-b-ok")
	if used != 2 {
		t.Errorf("got %d, wanted 2", used)
	}
	avail := ti.Available("default/network-class-b-ok")
	if avail != 65523 {
		t.Errorf("got %d, wanted 65523", avail)
	}

	// DeleteSubnet funtion tests
	ti.DeleteSubnet("default/network-class-c-ok")
	_, got := ti.GetIP("default/network-class-c-ok", "")
	if got == nil {
		t.Errorf("network default/network-class-c-ok still exists")
	} else if got.Error() != "network default/network-class-c-ok does not exists" {
		t.Errorf("got %q", got)
	}

	//
	// IPv6 tests
	//
	testSubnetsV6 := []struct {
		name   string
		subnet string
		start  string
		end    string
		want   error
	}{
		{
			name:   "default/network-ipv6-ok",
			subnet: "2001:db8::/64",
			start:  "2001:db8::10",
			end:    "2001:db8::ff",
			want:   nil,
		},
		{
			name:   "default/network-ipv6-start-error",
			subnet: "2001:db8::/64",
			start:  "2001:db9::10",
			end:    "2001:db8::ff",
			want:   fmt.Errorf("start address 2001:db9::10 is not within subnet 2001:db8::/64 range"),
		},
		{
			name:   "default/network-ipv6-end-error",
			subnet: "2001:db8::/64",
			start:  "2001:db8::10",
			end:    "2001:db9::ff",
			want:   fmt.Errorf("end address 2001:db9::ff is not within subnet 2001:db8::/64 range"),
		},
		{
			name:   "default/network-ipv6-smaller-error",
			subnet: "2001:db8::/64",
			start:  "2001:db8::ff",
			end:    "2001:db8::10",
			want:   fmt.Errorf("end address 2001:db8::10 is smaller then the start address 2001:db8::ff"),
		},
	}

	// NewSubnet function tests for IPv6
	for i := 0; i < len(testSubnetsV6); i++ {
		if got := ti.NewSubnet(
			testSubnetsV6[i].name,
			testSubnetsV6[i].subnet,
			testSubnetsV6[i].start,
			testSubnetsV6[i].end,
		); got != testSubnetsV6[i].want {
			if got == nil || testSubnetsV6[i].want == nil {
				t.Errorf("got %q, wanted %q", got, testSubnetsV6[i].want)
			} else if got.Error() != testSubnetsV6[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, testSubnetsV6[i].want)
			}
		}
	}

	allocateIPv6s := []struct {
		subnetName string
		ip         string
		want       error
	}{
		{
			subnetName: "default/network-ipv6-ok",
			ip:         "2001:db8::10",
			want:       nil,
		},
		{
			subnetName: "default/network-ipv6-ok",
			ip:         "2001:db8::10",
			want:       fmt.Errorf("given ip 2001:db8::10 is already allocated"),
		},
		{
			subnetName: "default/network-ipv6-ok",
			ip:         "",
			want:       nil, // should get next available: 2001:db8::11
		},
	}

	// GetIP function tests for IPv6
	for i := 0; i < len(allocateIPv6s); i++ {
		_, got := ti.GetIP(
			allocateIPv6s[i].subnetName,
			allocateIPv6s[i].ip,
		)
		if got != allocateIPv6s[i].want {
			if got == nil || allocateIPv6s[i].want == nil {
				t.Errorf("got %q, wanted %q", got, allocateIPv6s[i].want)
			} else if got.Error() != allocateIPv6s[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, allocateIPv6s[i].want)
			}
		}
	}

	releaseIPv6s := []struct {
		subnetName string
		ip         string
		want       error
	}{
		{
			subnetName: "default/network-ipv6-ok",
			ip:         "2001:db8::10",
			want:       nil,
		},
		{
			subnetName: "default/network-ipv6-ok",
			ip:         "2001:db8::10",
			want:       fmt.Errorf("given ip 2001:db8::10 was not allocated"),
		},
	}

	// ReleaseIP function tests for IPv6
	for i := 0; i < len(releaseIPv6s); i++ {
		if got := ti.ReleaseIP(
			releaseIPv6s[i].subnetName,
			releaseIPv6s[i].ip,
		); got != releaseIPv6s[i].want {
			if got == nil || releaseIPv6s[i].want == nil {
				t.Errorf("got %q, wanted %q", got, releaseIPv6s[i].want)
			} else if got.Error() != releaseIPv6s[i].want.Error() {
				t.Errorf("got %q, wanted %q", got, releaseIPv6s[i].want)
			}
		}
	}
}
