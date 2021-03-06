package dns

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/threefoldtech/zos/pkg/identity"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/threefoldtech/tfgateway/redis"
)

func Test_splitDomain(t *testing.T) {
	tests := []struct {
		domain string
		name   string
		zone   string
	}{
		{
			domain: "domain.com",
			name:   "",
			zone:   "domain.com",
		},
		{
			domain: "a.domain.com",
			name:   "a",
			zone:   "domain.com",
		},
		{
			domain: "a.b.c.domain.com",
			name:   "a",
			zone:   "b.c.domain.com",
		},
		{
			domain: "bleh.grid.deboeck.xyz",
			name:   "bleh",
			zone:   "grid.deboeck.xyz",
		},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			name, zone := splitDomain(tt.domain)
			assert.Equal(t, tt.name, name)
			assert.Equal(t, tt.zone, zone)
		})
	}
}

func Test_validateDomain(t *testing.T) {
	tests := []struct {
		domain string
		err    bool
	}{
		{
			domain: "domain.com",
			err:    false,
		},
		{
			domain: "a.domain.com",
			err:    false,
		},
		{
			domain: "a.b.c.domain.com",
			err:    false,
		},
		{
			domain: "bleh.grid.deboeck.xyz",
			err:    false,
		},
		{
			domain: "domain.com.",
			err:    true,
		},
		{
			domain: "foo",
			err:    true,
		},
		{
			domain: "",
			err:    true,
		},
		{
			domain: "foo..com",
			err:    true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.domain, func(t *testing.T) {
			err := validateDomain(tt.domain)
			if tt.err {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestRecordFromIP(t *testing.T) {
	tests := []struct {
		ip     net.IP
		record Record
	}{
		{ip: net.ParseIP("185.15.201.80"),
			record: RecordA{
				IP4: "185.15.201.80",
				TTL: 3600,
			},
		},
		{
			ip: net.ParseIP("2a02:2788:864:1314:9eb6:d0ff:fe97:764b"),
			record: RecordAAAA{
				IP6: "2a02:2788:864:1314:9eb6:d0ff:fe97:764b",
				TTL: 3600,
			},
		},
	}

	for _, tt := range tests {
		r := recordFromIP(tt.ip)
		assert.Equal(t, tt.record, r)
	}
}

func TestLoadRecords(t *testing.T) {
	z := Zone{}
	z.Add(RecordA{
		IP4: "142.93.229.35",
		TTL: 3600,
	})

	b, err := json.Marshal(z.Records)
	require.NoError(t, err)

	z2 := Zone{Records: records{}}
	err = json.Unmarshal(b, &z2.Records)
	require.NoError(t, err)
	require.Equal(t, 1, len(z.Records))
	assert.Equal(t, z.Records, z2.Records)
}

func TestZoneOwner(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)

	mgr := New(pool, "")

	zone := "mydomain.com"
	zo := ZoneOwner{Owner: "user1"}
	err = mgr.setZoneOwner(zone, zo)
	require.NoError(t, err, "setZoneOwner should succeed")

	result, err := mgr.getZoneOwner(zone)
	require.NoError(t, err)
	assert.Equal(t, zo, result, "getZoneOwner should return the stored ZoneOwner object")

	result, err = mgr.getZoneOwner("notexists")
	assert.NoError(t, err)
	assert.Equal(t, result.Owner, "", "non existing zone should return an empty owner")
}

func TestZoneRecords(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)

	mgr := New(pool, "")

	zone := "mydomain.com"
	name := "test"
	zo := Zone{
		Records: records{
			RecordTypeA: []Record{
				RecordA{
					IP4: "192.168.0.1",
					TTL: 3600,
				}},
			RecordTypeAAAA: []Record{
				RecordAAAA{
					IP6: "2a02:2788:864:1314:9eb6:d0ff:fe97:764b",
					TTL: 3600,
				},
			},
			RecordTypeTXT: []Record{
				RecordTXT{
					Text: "hello world",
					TTL:  3600,
				},
			},
		},
	}

	err = mgr.setZoneRecords(zone, name, zo)
	require.NoError(t, err, "setZoneRecords should succeed")

	result, err := mgr.getZoneRecords(zone, name)
	require.NoError(t, err)
	assert.Equal(t, zo, result, "getZoneRecords should return the same value that was set")

	output := s.HGet(zone+".", name)
	z := Zone{Records: records{}}

	err = json.Unmarshal([]byte(output), &z.Records)
	require.NoError(t, err)
	assert.Equal(t, zo, z)
}

func TestDomainDelegate(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)

	mgr := New(pool, "")

	id := "id"
	user := "user"
	domain := "my.domain.com"

	err = mgr.AddDomainDelagate(id, user, domain)
	require.NoError(t, err)

	err = mgr.RemoveDomainDelagate("user2", domain)
	assert.Error(t, err, "a domain can only be remove by its owner")
	assert.True(t, errors.Is(err, ErrAuth))

	err = mgr.AddDomainDelagate(id, "user2", domain)
	assert.Error(t, err, "a domain cannot be overwritten by another user")
	assert.True(t, errors.Is(err, ErrAuth))

	err = mgr.RemoveDomainDelagate(user, domain)
	require.NoError(t, err)
}

func TestSubdomain(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)
	mgr := New(pool, "")

	id := "id"
	user := "user"
	zone := "mydomain.com"
	domain := fmt.Sprintf("test.%s", zone)
	ips := []net.IP{
		net.ParseIP("10.1.1.10"),
	}

	err = mgr.AddDomainDelagate(id, user, zone)
	require.NoError(t, err)

	err = mgr.AddSubdomain(user, domain, ips)
	require.NoError(t, err)

	err = mgr.AddSubdomain("user2", domain, ips)
	require.Error(t, err, "only the owner of the zone can add a subdomain")
	assert.True(t, errors.Is(err, ErrAuth))

	err = mgr.RemoveSubdomain("user2", domain, ips)
	require.Error(t, err, "only the owner of the zone can remove a subdomain")
	assert.True(t, errors.Is(err, ErrAuth))

	err = mgr.RemoveSubdomain(user, domain, ips)
	require.NoError(t, err)

	err = mgr.AddSubdomain(user, "sub.thisisnotdelegated.com", ips)
	assert.Error(t, err)
	assert.Equal(t, "thisisnotdelegated.com is not managed by the gateway. delegate the domain first", err.Error())
}

func TestSubdomainChangeOwner(t *testing.T) {
	// https://github.com/threefoldtech/tfexplorer/issues/166
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)

	gwid := "gwid"
	mgr := New(pool, gwid)

	domain := "foo.mydomain.com"
	subdomain := fmt.Sprintf("test.%s", domain)
	ips := []net.IP{
		net.ParseIP("10.1.1.10"),
	}

	// the gateway manage a domain
	err = mgr.AddDomainDelagate("id", gwid, domain)
	require.NoError(t, err)

	// a user create a subdomain
	err = mgr.AddSubdomain("user", subdomain, ips)
	require.NoError(t, err)

	// free up the domain, anyone else should be able to use now
	err = mgr.RemoveSubdomain("user", subdomain, ips)
	require.NoError(t, err)

	err = mgr.AddSubdomain("user2", subdomain, ips)
	assert.NoError(t, err, "anyone should be able to use the domain again")
}

func TestManagedDomain(t *testing.T) {
	s, err := miniredis.Run()
	require.NoError(t, err)
	defer s.Close()

	kp, err := identity.GenerateKeyPair()
	require.NoError(t, err)

	pool, err := redis.NewPool(fmt.Sprintf("tcp://%s", s.Addr()))
	require.NoError(t, err)
	mgr := New(pool, kp.Identity())

	zone := "managed-domain.com"
	ips := []net.IP{
		net.ParseIP("10.1.1.10"),
	}

	// add the managed domain by the gateway
	err = mgr.AddDomainDelagate(kp.Identity(), kp.Identity(), zone)
	require.NoError(t, err)

	// random user add a subdomain on the managed domain
	err = mgr.AddSubdomain("user1", fmt.Sprintf("user1.%s", zone), ips)
	require.NoError(t, err)

	// random user add a subdomain on the managed domain
	err = mgr.AddSubdomain("user2", fmt.Sprintf("user2.%s", zone), ips)
	require.NoError(t, err)

	err = mgr.AddSubdomain("user2", fmt.Sprintf("user1.%s", zone), ips)
	require.Error(t, err, "a user cannot overwrite the domain of someone else")

	err = mgr.RemoveSubdomain("user2", fmt.Sprintf("user2.%s", zone), ips)
	require.NoError(t, err)

	ips = append(ips, net.ParseIP("2a02:2788:864:1314:9eb6:d0ff:fe97:764b"))
	err = mgr.AddSubdomain("user1", fmt.Sprintf("user1.%s", zone), ips)
	assert.Error(t, err, "a user cannot overwrite his domain without deletion first")

	err = mgr.AddSubdomain("user1", fmt.Sprintf("user2.%s", zone), ips)
	assert.NoError(t, err, "any user can reuse a freed subdomain")
}
