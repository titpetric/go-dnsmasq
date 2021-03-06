// Copyright (c) 2014 The SkyDNS Authors. All rights reserved.
// Copyright (c) 2015 Jan Broer
// Use of this source code is governed by The MIT License (MIT) that can be
// found in the LICENSE file.

package server

import (
	"strings"

	log "github.com/Sirupsen/logrus"
	"github.com/miekg/dns"
)

// ServeDNSForward forwards a request to the nameserver and returns the response.
func (s *server) ServeDNSForward(w dns.ResponseWriter, req *dns.Msg) *dns.Msg {
	if s.config.NoRec || len(s.config.Nameservers) == 0 {
		m := new(dns.Msg)
		m.SetReply(req)
		m.SetRcode(req, dns.RcodeRefused)
		m.Authoritative = false
		m.RecursionAvailable = false
		if len(s.config.Nameservers) == 0 {
			log.Debug("Not forwarding query, no nameservers configured")
		} else {
			log.Debug("Not forwarding query, recursive mode disabled")
		}

		w.WriteMsg(m)
		return m
	}

	name := req.Question[0].Name

	if dns.CountLabel(name) < s.config.Ndots {
		if !s.config.AppendDomain || len(s.config.SearchDomains) == 0 {
			log.Debugf("Not forwarding query, name too short: `%s'", name)
			m := new(dns.Msg)
			m.SetReply(req)
			m.SetRcode(req, dns.RcodeRefused)
			m.Authoritative = false
			m.RecursionAvailable = false
			w.WriteMsg(m)
			return m
		}
	}

	StatsForwardCount.Inc(1)

	var (
		r       *dns.Msg
		err     error
		nsList  []string
		nsIndex int // Nameserver list index
		sdIndex int // Search list index
		sdName  string // QNAME suffixed with search path
		sdCname = new(dns.CNAME) // CNAME record we include in replies for queries resolved by searching
	)

	tcp := isTCP(w)
	reqCopy := req.Copy()
	canSearch := false
	doingSearch := false

	if s.config.AppendDomain && len(s.config.SearchDomains) > 0 {
		canSearch = true
	}

Redo:
	if dns.CountLabel(name) < 2 {
		// Always qualify single-label names
		if !doingSearch && canSearch {
			doingSearch = true
			sdIndex = 0
		}
	}
	if doingSearch {
		sdName = strings.ToLower(appendDomain(name, s.config.SearchDomains[sdIndex]))
		sdCname.Hdr = dns.RR_Header{Name: name, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 360}
		sdCname.Target = sdName
		req.Question[0] = dns.Question{sdName, req.Question[0].Qtype, req.Question[0].Qclass}
	}

	nsList = s.config.Nameservers

	// Check whether the name matches a stub zone
	for zone, nss := range *s.config.Stub {
		if strings.HasSuffix(req.Question[0].Name, zone) {
			nsList = nss
			StatsStubForwardCount.Inc(1)
			break
		}
	}

	log.Debugf("Querying nameserver %s question %s", nsList[nsIndex], req.Question[0].Name)

	switch tcp {
	case false:
		r, _, err = s.dnsUDPclient.Exchange(req, nsList[nsIndex])
	case true:
		r, _, err = s.dnsTCPclient.Exchange(req, nsList[nsIndex])
	}
	if err == nil {
		if canSearch {
			// Replicate libc's getaddrinfo.c search logic
			switch {
			case r.Rcode == dns.RcodeSuccess && len(r.Answer) == 0 && !r.MsgHdr.Truncated: // NODATA !Truncated
				fallthrough
			case r.Rcode == dns.RcodeNameError: // NXDOMAIN
				fallthrough
			case r.Rcode == dns.RcodeServerFailure: // SERVFAIL
				if doingSearch && (sdIndex + 1) < len(s.config.SearchDomains) {
					// Continue searching
					sdIndex++
					goto Redo
				}
				if !doingSearch {
					// Start searching
					doingSearch = true
					sdIndex = 0
					goto Redo
				}
			}
		}

		if r.Rcode == dns.RcodeServerFailure || r.Rcode == dns.RcodeRefused {
			// Continue with next available nameserver
			if (nsIndex + 1) < len(nsList) {
				nsIndex++
				doingSearch = false
				goto Redo
			}	
		}

		// We are done querying. process the reply to return to the client.

		if doingSearch {
			// Insert cname record pointing name to name.searchdomain
			if len(r.Answer) > 0 {
				answers := []dns.RR{sdCname}
				for _, rr := range r.Answer {
					answers = append(answers, rr)
				}
				r.Answer = answers
			}
			// Restore original question
			r.Question[0] = reqCopy.Question[0]
		}

		r.Compress = true
		r.Id = req.Id
		w.WriteMsg(r)
		return r
	} else {
		log.Debugf("Error querying nameserver %s for qname %s: %q", nsList[nsIndex], name, err)
		// Got an error, this usually means the server did not respond
		// Continue with next available nameserver
		if (nsIndex + 1) < len(nsList) {
			nsIndex++
			doingSearch = false
			goto Redo
		}
	}

	// If we got here it means forwarding failed
	log.Errorf("Failed to forward query for qname %s: %q", name, err)
	m := new(dns.Msg)
	m.SetReply(reqCopy)
	m.SetRcode(reqCopy, dns.RcodeServerFailure)
	w.WriteMsg(m)
	return m
}

// ServeDNSReverse is the handler for DNS requests for the reverse zone. If nothing is found
// locally the request is forwarded to the forwarder for resolution.
func (s *server) ServeDNSReverse(w dns.ResponseWriter, req *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(req)
	m.Compress = true
	m.Authoritative = false
	m.RecursionAvailable = true
	if records, err := s.PTRRecords(req.Question[0]); err == nil && len(records) > 0 {
		m.Answer = records
		if err := w.WriteMsg(m); err != nil {
			log.Errorf("Failed to send reply: %q", err)
		}
		return m
	}
	// Always forward if not found locally.
	return s.ServeDNSForward(w, req)
}
