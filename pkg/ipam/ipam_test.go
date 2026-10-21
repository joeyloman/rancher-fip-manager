package ipam

import (
	"fmt"
	"sync"
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
			// Large but within MaxSubnetRangeSize.
			name:   "default/network-class-a-ok",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.10",
			end:    "10.0.255.254",
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
			// The broadcast check precedes the range bound, so the original
			// /8 case still reports the broadcast error unchanged.
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

// TestIpamConcurrency exercises concurrent allocations/releases against
// concurrent read-only access (Used/Available/Usage/GetUsage). Run with
// -race: before locking these readers, this reliably trips
// "concurrent map read and map write".
func TestIpamConcurrency(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("race/subnet", "192.168.100.0/24", "192.168.100.10", "192.168.100.110"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	const workers = 8
	const allocsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for range workers {
		go func() {
			defer wg.Done()

			var allocated []string
			for range allocsPerWorker {
				ip, err := ti.GetIP("race/subnet", "")
				if err == nil {
					allocated = append(allocated, ip)
				}

				// Read-only paths racing with allocations.
				_ = ti.Used("race/subnet")
				_ = ti.Available("race/subnet")
				_ = ti.GetUsage()
				ti.Usage("race/subnet")

				if err != nil && ti.Used("missing/subnet") != 0 {
					t.Errorf("Used on missing subnet should be 0")
				}
			}

			for _, ip := range allocated {
				if err := ti.ReleaseIP("race/subnet", ip); err != nil {
					t.Errorf("ReleaseIP(%s): %v", ip, err)
				}
			}
		}()
	}

	// Concurrent RefreshSubnet on another subnet exercises the writer path
	// against the readers.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for range 10 {
			if err := ti.RefreshSubnet("race/subnet2", "192.168.101.0/24", "192.168.101.10", "192.168.101.110", nil, nil); err != nil {
				t.Errorf("RefreshSubnet: %v", err)
			}
			_ = ti.Used("race/subnet2")
			_ = ti.GetUsage()
		}
	}()

	wg.Wait()

	if used := ti.Used("race/subnet"); used != 0 {
		t.Errorf("all IPs should be released, used=%d", used)
	}
	if got, err := ti.GetIP("race/subnet", "192.168.100.99"); err != nil || got != "192.168.100.99" {
		t.Errorf("GetIP after release: got (%q, %v), wanted %q", got, err, "192.168.100.99")
	}
}

// TestIpamOwnedConcurrency races the owners map: concurrent GetIPOwned /
// ReleaseIPOwned with per-worker owners plus read-only paths and a concurrent
// RefreshSubnet rebuild. Run under -race this turns the locking of the
// owner-tracking state from an inspected invariant into a checked one.
func TestIpamOwnedConcurrency(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("race/owned", "192.168.102.0/24", "192.168.102.10", "192.168.102.210"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	const workers = 8
	const allocsPerWorker = 50

	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		owner := fmt.Sprintf("ns/fip-%d", i)
		go func() {
			defer wg.Done()

			var allocated []string
			for range allocsPerWorker {
				ip, err := ti.GetIPOwned("race/owned", "", owner)
				if err == nil {
					allocated = append(allocated, ip)
				}

				// Read-only paths racing with owned allocations.
				_ = ti.Used("race/owned")
				_ = ti.Available("race/owned")
				_ = ti.GetUsage()
				ti.Usage("race/owned")
			}

			for _, ip := range allocated {
				if err := ti.ReleaseIPOwned("race/owned", ip, owner); err != nil {
					t.Errorf("ReleaseIPOwned(%s, %s): %v", owner, ip, err)
				}
			}
		}()
	}

	// Concurrent RefreshSubnet on another subnet races the owners-map rebuild
	// against owned claims and readers.
	wg.Add(1)
	go func() {
		defer wg.Done()

		for range 10 {
			if err := ti.RefreshSubnet("race/owned2", "192.168.103.0/24", "192.168.103.10", "192.168.103.110", nil, nil); err != nil {
				t.Errorf("RefreshSubnet: %v", err)
			}
			_ = ti.Used("race/owned2")
		}
	}()

	wg.Wait()

	if used := ti.Used("race/owned"); used != 0 {
		t.Errorf("all owned IPs should be released, used=%d", used)
	}
}

func TestIpamOwnedClaims(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("owned/subnet", "192.168.50.0/24", "192.168.50.10", "192.168.50.20"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	// Owner-aware claim of a specific IP.
	ip, err := ti.GetIPOwned("owned/subnet", "192.168.50.10", "ns/fip-a")
	if err != nil || ip != "192.168.50.10" {
		t.Fatalf("GetIPOwned: got (%q, %v), wanted (192.168.50.10, nil)", ip, err)
	}

	// Re-claim by the same owner is an idempotent no-op success.
	ip, err = ti.GetIPOwned("owned/subnet", "192.168.50.10", "ns/fip-a")
	if err != nil || ip != "192.168.50.10" {
		t.Errorf("idempotent re-claim: got (%q, %v), wanted (192.168.50.10, nil)", ip, err)
	}

	// A different owner must not steal the IP.
	if _, err := ti.GetIPOwned("owned/subnet", "192.168.50.10", "ns/fip-b"); err == nil {
		t.Errorf("cross-owner re-claim: expected error, got nil")
	}

	// An unowned claim (GetIP) must not be re-claimable by any owner.
	if _, err := ti.GetIP("owned/subnet", "192.168.50.11"); err != nil {
		t.Fatalf("GetIP: %v", err)
	}
	if _, err := ti.GetIPOwned("owned/subnet", "192.168.50.11", "ns/fip-a"); err == nil {
		t.Errorf("re-claim of unowned claim: expected error, got nil")
	}

	// Auto-allocation records the owner too.
	ip, err = ti.GetIPOwned("owned/subnet", "", "ns/fip-c")
	if err != nil || ip != "192.168.50.12" {
		t.Fatalf("auto GetIPOwned: got (%q, %v), wanted (192.168.50.12, nil)", ip, err)
	}

	// Release clears the owner; the IP becomes claimable by anyone.
	if err := ti.ReleaseIP("owned/subnet", "192.168.50.10"); err != nil {
		t.Fatalf("ReleaseIP: %v", err)
	}
	if _, err := ti.GetIPOwned("owned/subnet", "192.168.50.10", "ns/fip-b"); err != nil {
		t.Errorf("claim after release: unexpected error %v", err)
	}

	// Empty owner is rejected.
	if _, err := ti.GetIPOwned("owned/subnet", "192.168.50.13", ""); err == nil {
		t.Errorf("empty owner: expected error, got nil")
	}

	// ReleaseIPOwned refuses unowned claims (e.g. RefreshSubnet-restored
	// allocations that may back another FIP's persisted status), while plain
	// ReleaseIP still releases them (the main delete path keeps working).
	if _, err := ti.GetIP("owned/subnet", "192.168.50.14"); err != nil {
		t.Fatalf("GetIP: %v", err)
	}
	if err := ti.ReleaseIPOwned("owned/subnet", "192.168.50.14", "ns/fip-a"); err == nil {
		t.Errorf("ReleaseIPOwned on unowned claim: expected error, got nil")
	}
	if used := ti.Used("owned/subnet"); used != 4 {
		t.Errorf("unowned claim must survive the owned release attempt: used=%d, wanted 4", used)
	}
	if err := ti.ReleaseIP("owned/subnet", "192.168.50.14"); err != nil {
		t.Errorf("plain ReleaseIP on unowned claim: unexpected error %v", err)
	}
	if used := ti.Used("owned/subnet"); used != 3 {
		t.Errorf("plain ReleaseIP must release the unowned claim: used=%d, wanted 3", used)
	}
}

func TestRefreshSubnetPreservesOwnedClaims(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("refresh/owned", "192.168.60.0/24", "192.168.60.10", "192.168.60.20"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	// Claim granted but not yet visible in the pool-status-derived
	// allocations list (informer lag, the live duplicate-IP scenario).
	ip, err := ti.GetIPOwned("refresh/owned", "", "ns/fip-a")
	if err != nil {
		t.Fatalf("GetIPOwned: %v", err)
	}

	// A refresh built from a stale allocation list (empty here) must not
	// drop the in-flight owned claim.
	if err := ti.RefreshSubnet("refresh/owned", "192.168.60.0/24", "192.168.60.10", "192.168.60.20", nil, nil); err != nil {
		t.Fatalf("RefreshSubnet: %v", err)
	}

	// A second owner must not get the granted address...
	if _, err := ti.GetIPOwned("refresh/owned", ip, "ns/fip-b"); err == nil {
		t.Errorf("duplicate allocation after refresh: fip-b got %s already owned by fip-a", ip)
	}

	// ...while the owning FIP can still idempotently re-claim its own.
	if got, err := ti.GetIPOwned("refresh/owned", ip, "ns/fip-a"); err != nil || got != ip {
		t.Errorf("owner re-claim after refresh: got (%q, %v), wanted (%s, nil)", got, err, ip)
	}

	// Auto-allocation must also skip the carried-over claim.
	if ip2, err := ti.GetIPOwned("refresh/owned", "", "ns/fip-c"); err != nil || ip2 == ip {
		t.Errorf("auto-allocation after refresh: got (%q, %v), must differ from %s", ip2, err, ip)
	}

	// A claim that falls outside the range after shrinking is dropped
	// instead of failing the whole refresh.
	if _, err := ti.GetIPOwned("refresh/owned", "192.168.60.15", "ns/fip-d"); err != nil {
		t.Fatalf("GetIPOwned fip-d: %v", err)
	}
	if err := ti.RefreshSubnet("refresh/owned", "192.168.60.0/24", "192.168.60.10", "192.168.60.10", nil, nil); err != nil {
		t.Fatalf("RefreshSubnet shrink: %v", err)
	}
	if err := ti.ReleaseIPOwned("refresh/owned", "192.168.60.15", "ns/fip-d"); err == nil {
		t.Errorf("claim outside shrunk range should have been dropped, got successful release")
	}
	if used := ti.Used("refresh/owned"); used != 1 {
		t.Errorf("used=%d after shrink, wanted 1 (only the in-range carried claim)", used)
	}
}

// TestRefreshSubnetDropsOutOfRangeAllocations pins the shrink-below-owned
// wedge: a status-reported allocation outside the new range must NOT abort
// the swap (aborting kept the stale range alive while the spec said
// otherwise and error-looped the pool controller forever). The new range
// must be applied, in-range allocations preserved, and the dropped address
// must never be handed out again.
func TestRefreshSubnetDropsOutOfRangeAllocations(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("refresh/oor", "192.168.61.0/24", "192.168.61.10", "192.168.61.20"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	// .16 is owned in IPAM and also appears in the stale FIP-status-derived
	// allocations list; .11 is a normal persisted allocation.
	if _, err := ti.GetIPOwned("refresh/oor", "192.168.61.16", "ns/fip-a"); err != nil {
		t.Fatalf("GetIPOwned: %v", err)
	}
	if err := ti.RefreshSubnet("refresh/oor", "192.168.61.0/24", "192.168.61.10", "192.168.61.12",
		nil, []string{"192.168.61.11", "192.168.61.16"}); err != nil {
		t.Fatalf("RefreshSubnet with out-of-range allocations: %v", err)
	}

	usage, ok := ti.GetUsage()["refresh/oor"]
	if !ok {
		t.Fatalf("subnet missing from usage")
	}
	if usage.Start != "192.168.61.10" || usage.End != "192.168.61.12" || usage.Total != 3 {
		t.Errorf("new range not applied after shrink: start=%s end=%s total=%d", usage.Start, usage.End, usage.Total)
	}
	if usage.Used != 1 || len(usage.UsedIPs) != 1 || usage.UsedIPs[0] != "192.168.61.11" {
		t.Errorf("used=%d ips=%v, wanted exactly the in-range allocation 192.168.61.11", usage.Used, usage.UsedIPs)
	}

	// The dropped address is gone from the pool map: nobody can be handed
	// it again, not even by explicit request.
	if _, err := ti.GetIPOwned("refresh/oor", "192.168.61.16", "ns/fip-b"); err == nil {
		t.Errorf("out-of-range address was re-granted after the shrink")
	}
}

// TestReleaseAllByOwner pins the claim sweep for deletion: every claim
// tagged with the owner goes, foreign and unowned claims stay.
func TestReleaseAllByOwner(t *testing.T) {
	ti := New()
	if err := ti.NewSubnet("sweep/sub", "192.168.62.0/24", "192.168.62.10", "192.168.62.20"); err != nil {
		t.Fatalf("NewSubnet: %v", err)
	}

	abandoned1, err := ti.GetIPOwned("sweep/sub", "", "ns/fip-a")
	if err != nil {
		t.Fatalf("GetIPOwned abandoned1: %v", err)
	}
	abandoned2, err := ti.GetIPOwned("sweep/sub", "", "ns/fip-a")
	if err != nil {
		t.Fatalf("GetIPOwned abandoned2: %v", err)
	}
	if _, err := ti.GetIPOwned("sweep/sub", "", "ns/fip-b"); err != nil {
		t.Fatalf("GetIPOwned foreign: %v", err)
	}
	unowned, err := ti.GetIP("sweep/sub", "")
	if err != nil {
		t.Fatalf("GetIP unowned: %v", err)
	}

	if freed := ti.ReleaseAllByOwner("sweep/sub", "ns/fip-a"); freed != 2 {
		t.Errorf("freed=%d, wanted 2 (both abandoned claims)", freed)
	}
	if used := ti.Used("sweep/sub"); used != 2 {
		t.Errorf("used=%d after sweep, wanted 2 (foreign + unowned)", used)
	}
	if freed := ti.ReleaseAllByOwner("sweep/sub", "ns/fip-a"); freed != 0 {
		t.Errorf("second sweep freed=%d, wanted 0 (idempotent)", freed)
	}
	if freed := ti.ReleaseAllByOwner("sweep/sub", ""); freed != 0 {
		t.Errorf("empty owner freed=%d, wanted 0 (never sweep unowned)", freed)
	}
	if freed := ti.ReleaseAllByOwner("sweep/missing", "ns/fip-b"); freed != 0 {
		t.Errorf("missing subnet freed=%d, wanted 0", freed)
	}

	// Unowned claims are never reachable through an owner-based release.
	if err := ti.ReleaseIPOwned("sweep/sub", unowned, "ns/fip-a"); err == nil {
		t.Errorf("unowned claim must refuse owner-based release")
	}

	// The abandoned addresses are grantable again, and come back in pool
	// order (lowest freed address first).
	got, err := ti.GetIPOwned("sweep/sub", "", "ns/fip-c")
	if err != nil || got != abandoned1 {
		t.Errorf("re-allocation after sweep: got (%q,%v), wanted %s", got, err, abandoned1)
	}
	if got == abandoned2 {
		t.Errorf("re-allocation returned %q out of pool order", got)
	}
}

// TestRangeSizeBound pins the MaxSubnetRangeSize bound: buildSubnet used to materialize every
// address in [start,end] with no bound, so a large pool range (a /8, any
// sizeable IPv6 prefix) exhausted memory while holding the IPAM mutex. The
// range is now bounded at MaxSubnetRangeSize addresses, checked before the
// allocation map is built, and surfaced through the pool admission webhook.
func TestRangeSizeBound(t *testing.T) {
	ti := New()

	testSubnets := []struct {
		name   string
		subnet string
		start  string
		end    string
		want   error
	}{
		{
			// Exactly MaxSubnetRangeSize addresses (inclusive range) fits.
			name:   "range-at-maximum-size-ok",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.0",
			end:    "10.0.255.255",
			want:   nil,
		},
		{
			// One address beyond the bound must be rejected before
			// materialization.
			name:   "range-over-maximum-size-error",
			subnet: "10.0.0.0/8",
			start:  "10.0.0.0",
			end:    "10.1.0.0",
			want:   fmt.Errorf("pool range 10.0.0.0-10.1.0.0 spans 65537 addresses, exceeding the maximum of %d", MaxSubnetRangeSize),
		},
		{
			// An IPv6 range far beyond int64 must be rejected, not loop.
			name:   "ipv6-range-over-maximum-size-error",
			subnet: "2001:db8::/32",
			start:  "2001:db8::1",
			end:    "2001:db8:ffff:ffff::1",
			want:   fmt.Errorf("pool range 2001:db8::1-2001:db8:ffff:ffff::1 spans 79228162495817593519834398721 addresses, exceeding the maximum of %d", MaxSubnetRangeSize),
		},
		{
			// Same-/64 endpoints whose span (2^64-1) overflows int64: the
			// big.Int difference must not wrap past the bound.
			name:   "ipv6-same-slash64-over-maximum-size-error",
			subnet: "2001:db8::/64",
			start:  "2001:db8::1",
			end:    "2001:db8::ffff:ffff:ffff:ffff",
			want:   fmt.Errorf("pool range 2001:db8::1-2001:db8::ffff:ffff:ffff:ffff spans 18446744073709551615 addresses, exceeding the maximum of %d", MaxSubnetRangeSize),
		},
	}

	for i := 0; i < len(testSubnets); i++ {
		got := ti.NewSubnet(
			testSubnets[i].name,
			testSubnets[i].subnet,
			testSubnets[i].start,
			testSubnets[i].end,
		)
		if got == nil && testSubnets[i].want != nil {
			t.Errorf("%s: got nil, wanted %q", testSubnets[i].name, testSubnets[i].want)
		} else if got != nil && testSubnets[i].want == nil {
			t.Errorf("%s: got %q, wanted nil", testSubnets[i].name, got)
		} else if got != nil && got.Error() != testSubnets[i].want.Error() {
			t.Errorf("%s: got %q, wanted %q", testSubnets[i].name, got, testSubnets[i].want)
		}
	}

	// The rejected subnets must not have been registered: a later allocation
	// attempt reports the subnet as absent instead of handing out addresses
	// from an unbuilt range.
	if _, err := ti.GetIP("range-over-maximum-size-error", ""); err == nil {
		t.Errorf("oversized subnet was registered despite build error")
	}

	// ValidateRangeSize is the exported bound used by the webhook; it must
	// accept the at-limit range and parse errors must surface.
	if err := ValidateRangeSize("10.0.0.0", "10.0.255.255"); err != nil {
		t.Errorf("at-limit range rejected: %v", err)
	}
	if err := ValidateRangeSize("not-an-ip", "10.0.0.1"); err == nil {
		t.Errorf("invalid start address accepted")
	}
	if err := ValidateRangeSize("10.0.0.1", "not-an-ip"); err == nil {
		t.Errorf("invalid end address accepted")
	}
}
