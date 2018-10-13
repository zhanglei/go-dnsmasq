package main

import (
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/semihalev/log"
)

// Resolver type
type Resolver struct {
	config  *dns.ClientConfig
	nsCache *NameServerCache
	rCache  *MemoryCache
}

var (
	rootzone = "."

	rootservers = []string{
		"198.41.0.4:53",
		"192.228.79.201:53",
		"192.33.4.12:53",
		"199.7.91.13:53",
		"192.203.230.10:53",
		"192.5.5.241:53",
		"192.112.36.4:53",
		"128.63.2.53:53",
		"192.36.148.17:53",
		"192.58.128.30:53",
		"193.0.14.129:53",
		"199.7.83.42:53",
		"202.12.27.33:53",
	}

	initialkeys = []string{
		".			172800	IN	DNSKEY	257 3 8 AwEAAagAIKlVZrpC6Ia7gEzahOR+9W29euxhJhVVLOyQbSEW0O8gcCjFFVQUTf6v58fLjwBd0YI0EzrAcQqBGCzh/RStIoO8g0NfnfL2MTJRkxoXbfDaUeVPQuYEhg37NZWAJQ9VnMVDxP/VHL496M/QZxkjf5/Efucp2gaDX6RS6CXpoY68LsvPVjR0ZSwzz1apAzvN9dlzEheX7ICJBBtuA6G3LQpzW5hOA2hzCTMjJPJ8LbqF6dsV6DoBQzgul0sGIcGOYl7OyQdXfZ57relSQageu+ipAdTTJ25AsRTAoub8ONGcLmqrAmRLKBP1dfwhYB4N7knNnulqQxA+Uk1ihz0=",
		".			172800	IN	DNSKEY	256 3 8 AwEAAdp440E6Mz7c+Vl4sPd0lTv2Qnc85dTW64j0RDD7sS/zwxWDJ3QRES2VKDO0OXLMqVJSs2YCCSDKuZXpDPuf++YfAu0j7lzYYdWTGwyNZhEaXtMQJIKYB96pW6cRkiG2Dn8S2vvo/PxW9PKQsyLbtd8PcwWglHgReBVp7kEv/Dd+3b3YMukt4jnWgDUddAySg558Zld+c9eGWkgWoOiuhg4rQRkFstMX1pRyOSHcZuH38o1WcsT4y3eT0U/SR6TOSLIB/8Ftirux/h297oS7tCcwSPt0wwry5OFNTlfMo8v7WGurogfk8hPipf7TTKHIi20LWen5RCsvYsQBkYGpF78=",
	}

	rootkeys = []dns.RR{}
)

func init() {
	for _, k := range initialkeys {
		rr, err := dns.NewRR(k)
		if err != nil {
			panic(err)
		}
		rootkeys = append(rootkeys, rr)
	}
}

// NewResolver return a resolver
func NewResolver() *Resolver {
	return &Resolver{
		&dns.ClientConfig{},
		NewNameServerCache(Config.Maxcount),
		NewMemoryCache(Config.Maxcount),
	}
}

// Resolve will ask each nameserver in top-to-bottom fashion, starting a new request
// in every interval, and return as early as possbile (have an answer).
// It returns an error if no request has succeeded.
func (r *Resolver) Resolve(Net string, req *dns.Msg, servers []string, root bool, depth int, level int, nsl bool, parentdsrr []dns.RR) (resp *dns.Msg, err error) {
	if depth == 0 {
		return resp, fmt.Errorf("maximum recursion depth for DNS tree queried")
	}

	if root {
		q := req.Question[0]
		servers, parentdsrr = r.searchCache(&q)
	}

	resp, err = r.lookup(Net, req, servers)

	if err != nil {
		return
	}

	if len(resp.Answer) > 0 {
		signer := req.Question[0].Name
		signerFound := false

		for _, rr := range resp.Answer {
			if sigrec, ok := rr.(*dns.RRSIG); ok {
				signer = sigrec.SignerName
				signerFound = true
				break
			}
		}

		if signerFound && len(parentdsrr) > 0 {
			if dsrr, ok := parentdsrr[0].(*dns.DS); ok {
				if dsrr.Header().Name != signer && req.Question[0].Qtype != dns.TypeDS {
					//try lookup DS records
					dsReq := new(dns.Msg)
					dsReq.SetQuestion(signer, dns.TypeDS)
					dsReq.SetEdns0(edns0size, true)

					dsDepth := Config.Maxdepth
					dsResp, err := r.Resolve(Net, dsReq, rootservers, true, dsDepth, 0, false, nil)
					if err == nil {
						parentdsrr = extractRRSet(dsResp.Answer, signer, dns.TypeDS)
					} else {
						signerFound = false
					}
				}
			}
		}

		if signerFound && (signer == rootzone || len(parentdsrr) > 0) {
			err := r.verifyDNSSEC(Net, signer, resp, parentdsrr, servers)
			if err != nil {
				log.Info("DNSSEC verify failed", "qname", req.Question[0].Name, "qtype", dns.TypeToString[req.Question[0].Qtype], "error", err.Error())
				return nil, err
			}
		}

		resp.Ns = []dns.RR{}
		resp.Extra = []dns.RR{}

		return
	}

	if len(resp.Answer) == 0 && len(resp.Ns) > 0 {
		if nsrec, ok := resp.Ns[0].(*dns.NS); ok {
			nlevel := len(strings.Split(nsrec.Header().Name, rootzone))
			if level > nlevel {
				return resp, fmt.Errorf("parent detection")
			}

			Q := Question{unFqdn(nsrec.Header().Name), dns.TypeToString[nsrec.Header().Rrtype], dns.ClassToString[nsrec.Header().Class]}
			if Q.Qname == "" {
				return resp, fmt.Errorf("root servers detection")
			}

			key := keyGen(Q)

			ns, err := r.nsCache.Get(key)
			if err == nil {
				if reflect.DeepEqual(ns.Servers, servers) {
					return resp, fmt.Errorf("loop detection")
				}

				log.Debug("Nameserver cache hit", "key", key, "query", Q.String())

				depth--
				return r.Resolve(Net, req, ns.Servers, false, depth, nlevel, nsl, nil)
			}

			log.Debug("Nameserver cache not found", "key", key, "query", Q.String(), "error", err.Error())
		}

		ns := make(map[string]string)
		for _, n := range shuffleRR(resp.Ns) {
			if nsrec, ok := n.(*dns.NS); ok {
				ns[nsrec.Ns] = ""
			}
		}

		for _, a := range resp.Extra {
			if extra, ok := a.(*dns.A); ok {
				if nsl && extra.Header().Name == req.Question[0].Name && extra.A.String() != "" {
					resp.Answer = append(resp.Answer, extra)
					log.Debug("Glue NS addr in extra response", "qname", extra.Header().Name, "a", extra.A.String())
					return
				}

				if _, ok := ns[extra.Header().Name]; ok {
					ns[extra.Header().Name] = extra.A.String()
				}
			}
		}

		nservers := []string{}
		for _, addr := range ns {
			if addr != "" {
				if isLocalIP(addr) {
					continue
				}
				nservers = append(nservers, fmt.Sprintf("%s:53", addr))
			}
		}

		for k, addr := range ns {
			if addr == "" {
				//FIX: temprorary, need fix loops and change to inside resolver
				addr, err := r.lookupNSAddr(Net, k, []string{"8.8.8.8:53", "8.8.4.4:53"})

				if err == nil {
					if isLocalIP(addr) {
						continue
					}
					nservers = append(nservers, fmt.Sprintf("%s:53", addr))
				}
			}
		}

		if len(nservers) == 0 {
			return
		}

		if nsrec, ok := resp.Ns[0].(*dns.NS); ok {
			nlevel := len(strings.Split(nsrec.Header().Name, rootzone))
			if level > nlevel {
				return resp, fmt.Errorf("parent detection")
			}

			Q := Question{unFqdn(nsrec.Header().Name), dns.TypeToString[nsrec.Header().Rrtype], dns.ClassToString[nsrec.Header().Class]}
			if Q.Qname == "" {
				return resp, fmt.Errorf("root servers detection")
			}

			signer := upperName(nsrec.Header().Name)
			if signer == "" {
				signer = rootzone
			}

			for _, rr := range resp.Ns {
				if sigrec, ok := rr.(*dns.RRSIG); ok {
					signer = sigrec.SignerName
					break
				}
			}

			if signer == rootzone || len(parentdsrr) > 0 {
				err := r.verifyDNSSEC(Net, signer, resp, parentdsrr, servers)
				if err != nil {
					log.Info("DNSSEC verify failed (not cached)", "qname", req.Question[0].Name, "qtype", dns.TypeToString[req.Question[0].Qtype], "error", err.Error())
					return nil, err
				}

				parentdsrr = extractRRSet(resp.Ns, nsrec.Header().Name, dns.TypeDS)
			}

			key := keyGen(Q)

			err := r.nsCache.Set(key, parentdsrr, nsrec.Header().Ttl, nservers)
			if err != nil {
				log.Error("Set nameserver cache failed", "query", Q.String(), "error", err.Error())
			} else {
				log.Debug("Nameserver cache insert", "key", key, "query", Q.String())
			}

			depth--
			return r.Resolve(Net, req, nservers, false, depth, nlevel, nsl, parentdsrr)
		}
	}

	return
}

func (r *Resolver) lookup(Net string, req *dns.Msg, servers []string) (resp *dns.Msg, err error) {
	c := &dns.Client{
		Net:          Net,
		UDPSize:      dns.DefaultMsgSize,
		Dialer:       &net.Dialer{Timeout: time.Duration(Config.ConnectTimeout) * time.Second},
		ReadTimeout:  time.Duration(Config.Timeout) * time.Second,
		WriteTimeout: time.Duration(Config.Timeout) * time.Second,
	}

	if Config.OutboundIP != "" {
		if Net == "tcp" {
			c.Dialer.LocalAddr = &net.TCPAddr{IP: net.ParseIP(Config.OutboundIP)}
		} else if Net == "udp" {
			c.Dialer.LocalAddr = &net.UDPAddr{IP: net.ParseIP(Config.OutboundIP)}
		}
	}

	qname := req.Question[0].Name
	qtype := dns.Type(req.Question[0].Qtype).String()

	res := make(chan *dns.Msg)

	var wg sync.WaitGroup

	L := func(server string, last bool) {
		defer wg.Done()

		r, _, err := c.Exchange(req, server)
		if err != nil && err != dns.ErrTruncated {
			log.Info("Got an error from resolver", "qname", qname, "qtype", qtype, "server", server, "net", Net, "error", err.Error())
			return
		}

		if r != nil && r.Rcode != dns.RcodeSuccess && !last {
			log.Debug("Failed to get valid response", "qname", qname, "qtype", qtype, "server", server, "net", Net, "rcode", dns.RcodeToString[r.Rcode])
			return
		}

		log.Debug("Query response", "qname", unFqdn(qname), "qtype", qtype, "server", server, "net", Net, "rcode", dns.RcodeToString[r.Rcode])

		select {
		case res <- r:
		default:
		}
	}

	ticker := time.NewTicker(time.Duration(Config.Interval) * time.Millisecond)
	defer ticker.Stop()

	// Start lookup on each nameserver top-down, in interval
	for index, server := range servers {
		wg.Add(1)
		go L(server, len(servers)-1 == index)

		// but exit early, if we have an answer
		select {
		case r := <-res:
			return r, nil
		case <-ticker.C:
			continue
		}
	}

	// wait for all the namservers to finish
	wg.Wait()

	select {
	case r := <-res:
		return r, nil
	case <-time.After(time.Duration(Config.Timeout*len(servers)) * time.Second):
		return nil, fmt.Errorf("timedout")
	default:
		return nil, fmt.Errorf("resolv failed")
	}
}

func (r *Resolver) searchCache(q *dns.Question) (servers []string, parentdsrr []dns.RR) {
	Q := Question{unFqdn(q.Name), dns.TypeToString[dns.TypeNS], dns.ClassToString[q.Qclass]}
	key := keyGen(Q)

	ns, err := r.nsCache.Get(key)

	if err == nil {
		log.Debug("Nameserver cache hit", "key", key, "query", Q.String())
		return ns.Servers, ns.DSRR
	}

	q.Name = upperName(q.Name)

	if q.Name == "" {
		return rootservers, nil
	}

	return r.searchCache(q)
}

func (r *Resolver) lookupNSAddr(Net string, ns string, servers []string) (addr string, err error) {
	nsReq := new(dns.Msg)
	nsReq.SetQuestion(ns, dns.TypeA)
	nsReq.RecursionDesired = true

	q := nsReq.Question[0]
	Q := Question{unFqdn(q.Name), dns.TypeToString[q.Qtype], dns.ClassToString[q.Qclass]}

	key := keyGen(Q)

	nsres, _, err := r.rCache.Get(key)
	if err == nil {
		if addr, ok := searchAddr(nsres); ok {
			return addr, nil
		}
	}

	if len(servers) == 0 {
		depth := Config.Maxdepth
		nsres, err = r.Resolve(Net, nsReq, rootservers, true, depth, 0, true, nil)
	} else {
		nsres, err = r.lookup(Net, nsReq, servers)
	}

	if err != nil {
		log.Debug("NS record failed", "qname", Q.Qname, "qtype", Q.Qtype, "error", err.Error())
		return
	}

	if addr, ok := searchAddr(nsres); ok {
		r.rCache.Set(key, nsres)
		return addr, nil
	}

	return addr, fmt.Errorf("ns addr not found %s", ns)
}

func (r *Resolver) verifyDNSSEC(Net string, qname string, resp *dns.Msg, parentdsRR []dns.RR, servers []string) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%v for parentname=%s qname=%s", err, qname, resp.Question[0].Name)
		}
	}()

	req := new(dns.Msg)
	req.SetQuestion(qname, dns.TypeDNSKEY)
	req.SetEdns0(edns0size, true)

	q := req.Question[0]
	Q := Question{unFqdn(q.Name), dns.TypeToString[q.Qtype], dns.ClassToString[q.Qclass]}

	cacheKey := keyGen(Q)
	msg, _, err := r.rCache.Get(cacheKey)
	if msg == nil {
		if qname == rootzone {
			msg = new(dns.Msg)
			msg.SetQuestion(".", dns.TypeDNSKEY)

			msg.Answer = append(msg.Answer, rootkeys...)
		} else {
			msg, err = r.lookup(Net, req, servers)
			if err != nil {
				return
			}
		}
	}

	keys := make(map[uint16]*dns.DNSKEY)
	for _, a := range msg.Answer {
		if a.Header().Rrtype == dns.TypeDNSKEY {
			dnskey := a.(*dns.DNSKEY)
			tag := dnskey.KeyTag()
			if dnskey.Flags == 256 || dnskey.Flags == 257 {
				keys[tag] = dnskey
			}
		}
	}

	if len(keys) == 0 {
		return errNoDNSKEY
	}

	if len(parentdsRR) > 0 {
		err = verifyDS(keys, parentdsRR)
		if err != nil {
			log.Debug("DNSSEC DS verify failed", "qname", qname, "error", err.Error())
			return
		}
	}

	if err = verifyRRSIG(keys, resp); err != nil {
		return
	}

	r.rCache.Set(cacheKey, msg)

	log.Debug("DNSSEC verified", "parent", qname, "qname", resp.Question[0].Name)

	return nil
}
