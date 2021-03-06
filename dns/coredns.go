package dns

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"

	"github.com/asaskevich/govalidator"
	"github.com/pkg/errors"
	"github.com/rs/zerolog/log"

	"github.com/gomodule/redigo/redis"
)

// Mgr is responsible to configure CoreDNS trough its redis pluging
type Mgr struct {
	redis    *redis.Pool
	identity string
}

// New creates a DNS manager
func New(pool *redis.Pool, identity string) *Mgr {
	return &Mgr{
		redis:    pool,
		identity: identity,
	}
}

// Cleanup makes sure that currect coredns configuration
// is optimal by cleaning up not used records
func (c *Mgr) Cleanup() error {
	zones, err := c.listCorednsZones()
	if err != nil {
		return err
	}

	for _, zone := range zones {
		if err := c.cleanUp(zone); err != nil {
			log.Error().Err(err).Str("zone", zone).Msg("failed to cleanup zone")
		}
	}

	return nil
}

func (c *Mgr) cleanUp(zone string) error {
	con := c.redis.Get()
	defer con.Close()

	keys, err := redis.Strings(con.Do("HKEYS", zone))
	if err != nil {
		return errors.Wrapf(err, "failed to list keys of zone '%s'", zone)
	}

	for _, key := range keys {
		value, err := redis.String(con.Do("HGET", zone, key))
		if err != nil {
			log.Error().Err(err).Str("zone", zone).Str("key", key).Msg("failed to get value")
			continue
		}

		if len(value) == 0 || value == "{}" {
			if _, err := con.Do("HDEL", zone, key); err != nil {
				log.Error().Err(err).Str("zone", zone).Str("key", key).Msg("failed to delete empty key")
			}
		}
	}

	return nil
}

func (c *Mgr) listCorednsZones() ([]string, error) {
	con := c.redis.Get()
	defer con.Close()

	zones, err := redis.Strings(con.Do("KEYS", "*."))
	if err != nil {
		return nil, errors.Wrap(err, "failed to list potential coredns zones")
	}

	result := zones[:0]
	for _, zone := range zones {
		typ, err := redis.String(con.Do("TYPE", zone))
		if err != nil {
			return nil, errors.Wrapf(err, "failed to get type of redis key '%s'", zone)
		}

		if typ == "hash" {
			result = append(result, zone)
		}
	}

	return result, nil
}

func (c *Mgr) getZoneOwner(zone string) (owner ZoneOwner, err error) {
	zone = strings.TrimSuffix(zone, ".")

	con := c.redis.Get()
	defer con.Close()

	data, err := redis.Bytes(con.Do("HGET", "zone", zone))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			return owner, nil
		}
		return owner, fmt.Errorf("failed to read the DNS zone %s: %w", zone, err)
	}

	if err := json.Unmarshal(data, &owner); err != nil {
		return owner, err
	}
	return owner, nil
}

func (c *Mgr) setZoneOwner(zone string, owner ZoneOwner) (err error) {
	con := c.redis.Get()
	defer con.Close()

	b, err := json.Marshal(owner)
	if err != nil {
		return err
	}

	_, err = con.Do("HSET", "zone", zone, b)
	return err
}

func (c *Mgr) getZoneRecords(zone, name string) (Zone, error) {
	con := c.redis.Get()
	defer con.Close()

	if zone[len(zone)-1] != '.' {
		zone += "."
	}

	zr := Zone{Records: records{}}
	data, err := redis.Bytes(con.Do("HGET", zone, name))
	if err != nil {
		if errors.Is(err, redis.ErrNil) {
			return zr, nil
		}
		return zr, fmt.Errorf("failed to read the DNS zone %s: %w", zone, err)
	}

	if err := json.Unmarshal(data, &zr.Records); err != nil {
		return zr, err
	}
	log.Debug().Msgf("get zone records %+v", zr)
	return zr, nil
}

func (c *Mgr) setZoneRecords(zone, name string, zr Zone) (err error) {
	log.Debug().Msgf("zet zone records %+v", zr)
	con := c.redis.Get()
	defer con.Close()

	if zone[len(zone)-1] != '.' {
		zone += "."
	}

	b, err := json.Marshal(zr.Records)
	if err != nil {
		return err
	}

	if _, err := con.Do("HSET", zone, name, b); err != nil {
		return err
	}

	return nil
}

func (c *Mgr) deleteZoneRecords(zone, name string) (err error) {
	log.Debug().Str("name", name).Str("zone", zone).Msg("delete zone record")
	con := c.redis.Get()
	defer con.Close()

	if zone[len(zone)-1] != '.' {
		zone += "."
	}

	if _, err := con.Do("HDEL", zone, name); err != nil {
		return err
	}

	return nil
}

func (c *Mgr) setSubdomainOwner(domain, user string) error {
	log.Debug().Msgf("set managed domain owner %s %s", domain, user)
	con := c.redis.Get()
	defer con.Close()

	if _, err := con.Do("HSET", "managed_domains", domain, user); err != nil {
		return err
	}

	return nil
}

func (c *Mgr) getSubdomainOwner(domain string) (user string, err error) {
	log.Debug().Msgf("get managed domain owner %s %s", domain, user)
	con := c.redis.Get()
	defer con.Close()

	user, err = redis.String(con.Do("HGET", "managed_domains", domain))
	if err == redis.ErrNil {
		return "", nil
	} else if err != nil {
		return "", err
	}

	return user, nil
}

func (c *Mgr) deleteSubdomainOwner(domain string) error {
	log.Debug().Msgf("delete managed domain owner %s", domain)
	con := c.redis.Get()
	defer con.Close()

	_, err := con.Do("HDEL", "managed_domains", domain)
	return err
}

// AddSubdomain configures a domain A or AAA records depending on the version of
// the IP address in IPs
func (c *Mgr) AddSubdomain(user string, domain string, IPs []net.IP) error {

	log.Info().Msgf("add subdomain %s %+v", domain, IPs)

	if err := validateDomain(domain); err != nil {
		return err
	}

	name, zone := splitDomain(domain)

	con := c.redis.Get()
	defer con.Close()

	owner, err := c.getZoneOwner(zone)
	if err != nil {
		return fmt.Errorf("failed to read the DNS zone %s: %w", zone, err)
	}

	if owner.Owner == "" {
		return fmt.Errorf("%s is not managed by the gateway. delegate the domain first", zone)
	}

	if owner.Owner == c.identity { // this is a manged domain
		owner, err := c.getSubdomainOwner(domain)
		if err != nil {
			return err
		}

		if owner != "" {
			// the sub-domain is already provisioned, so regardless it's by the own user
			// or not, the user need to first deprovision it, before he can use it again.
			//return errors.

			return errors.Wrapf(ErrSubdomainUsed, "cannot add subdomain %s to zone %s", name, zone)
		}
	} else if owner.Owner != user { //this is a deletegatedDomain
		return errors.Wrapf(ErrAuth, "cannot add subdomain %s to zone %s", name, zone)
	}

	// we mark this subdomain as reserved for that user
	if err := c.setSubdomainOwner(domain, user); err != nil {
		return errors.Wrap(err, "failed to reserve subdomain")
	}

	defer func() {
		if err != nil {
			if err := c.deleteSubdomainOwner(domain); err != nil {
				log.Error().Err(err).Msg("failed to clean up sub-domain reservation owner")
			}
		}
	}()

	zr, err := c.getZoneRecords(zone, name)
	if err != nil {
		return err
	}

	for _, ip := range IPs {
		r := recordFromIP(ip)
		zr.Add(r)
	}

	if err = c.setZoneRecords(zone, name, zr); err != nil {
		return err
	}

	return nil
}

// RemoveSubdomain remove a domain added with AddSubdomain
func (c *Mgr) RemoveSubdomain(user string, domain string, IPs []net.IP) error {
	if err := validateDomain(domain); err != nil {
		return err
	}

	name, zone := splitDomain(domain)

	con := c.redis.Get()
	defer con.Close()

	owner, err := c.getZoneOwner(zone)
	if err != nil {
		return fmt.Errorf("failed to read the DNS zone %s: %w", zone, err)
	}

	if owner.Owner == "" {
		// domain not managed by this gateway at all, so all subdomain are already gone too.
		// this can happen when a delegated domain expires before a subdomain

		// we can safely then delete the subdomain owner
		// as a way of clean up. (records already gone with the domain)
		return c.deleteSubdomainOwner(domain)
	}

	// this is now set for both managed domains and delegated domains
	// if the owner name is not set we still continue (backward compatibility)
	// otherwise we check if it matches the user
	ownerName, err := c.getSubdomainOwner(domain)
	if err != nil {
		return err
	}
	if ownerName != "" && ownerName != user {
		return errors.Wrapf(ErrAuth, "cannot remove subdomain %s from zone %s", name, zone)
	}

	zr, err := c.getZoneRecords(zone, name)
	if err != nil {
		return err
	}

	if zr.Records.IsEmpty() {
		return nil
	}

	for _, ip := range IPs {
		r := recordFromIP(ip)
		zr.Remove(r)
	}

	if zr.Records.IsEmpty() {
		if err := c.deleteZoneRecords(zone, name); err != nil {
			return err
		}
		// if the subdomain has been cleared out, we remove the owner so anyone can claim it again
		return c.deleteSubdomainOwner(domain)

	}

	return c.setZoneRecords(zone, name, zr)
}

// AddDomainDelagate configures coreDNS to manage domain
func (c *Mgr) AddDomainDelagate(identity, user, domain string) error {
	if err := validateDomain(domain); err != nil {
		return err
	}

	owner, err := c.getZoneOwner(domain)
	if err != nil {
		return err
	}

	if owner.Owner != "" && owner.Owner != user {
		return fmt.Errorf("%w cannot delegate domain %s", ErrAuth, domain)
	}

	owner.Owner = user
	if err := c.setZoneOwner(domain, owner); err != nil {
		return errors.Wrap(err, "failed to set zone owner")
	}

	return c.setZoneOwnerTXTRecord(domain, identity, owner.Owner)
}

func (c *Mgr) setZoneOwnerTXTRecord(domain, identity, owner string) error {
	const name = "__owner__"
	var zone Zone
	// we are not using the ZoneOwner struct because of
	// 1- backward compatibility issue since it does not define json tags
	// 2- extendability of this struct with more data in the future
	data := struct {
		Identity string `json:"identity"`
		Owner    string `json:"owner"`
	}{
		Identity: identity,
		Owner:    owner,
	}

	bytes, err := json.Marshal(data)
	if err != nil {
		return errors.Wrap(err, "failed to create owner TXT record")
	}

	zone.Add(RecordTXT{Text: string(bytes), TTL: 600})

	return c.setZoneRecords(domain, name, zone)
}

// RemoveDomainDelagate remove a delagated domain added with AddDomainDelagate
func (c *Mgr) RemoveDomainDelagate(user string, domain string) error {
	if err := validateDomain(domain); err != nil {
		return err
	}

	owner, err := c.getZoneOwner(domain)
	if err != nil {
		return err
	}

	if owner.Owner != "" && owner.Owner != user {
		return fmt.Errorf("%w cannot remove delegated domain %s", ErrAuth, domain)
	}

	con := c.redis.Get()
	defer con.Close()

	// TODO IMPORTANT: delete all sub-domain owners
	// we need to go over all managed_domains
	// do hkeys managed_domains, find all keys that has domain as suffix
	// delete

	// remove all eventual subdomain configuration for this delegated domain
	if _, err = con.Do("DEL", domain); err != nil {
		return err
	}

	_, err = con.Do("HDEL", "zone", domain)
	return err
}

func splitDomain(d string) (name, domain string) {
	ss := strings.Split(d, ".")
	if len(ss) < 3 {
		return "", d
	}
	return ss[0], strings.Join(ss[1:], ".")
}

func recordFromIP(ip net.IP) (r Record) {
	if ip.To4() != nil {
		r = RecordA{
			IP4: ip.String(),
			TTL: 3600,
		}
	} else {
		r = RecordAAAA{
			IP6: ip.String(),
			TTL: 3600,
		}
	}
	return r
}

func validateDomain(domain string) error {
	if !govalidator.IsDNSName(domain) {
		return fmt.Errorf("domain '%s' name is invalid", domain)
	}

	if len(domain) == 0 {
		return fmt.Errorf("incorrect format for domain %s", domain)
	}

	if strings.Count(domain, ".") < 1 {
		return fmt.Errorf("incorrect format for domain %s", domain)
	}

	if domain[len(domain)-1] == '.' {
		return fmt.Errorf("incorrect format for domain %s", domain)
	}

	if strings.Contains(domain, "..") {
		return fmt.Errorf("incorrect format for domain %s", domain)

	}

	return nil
}
