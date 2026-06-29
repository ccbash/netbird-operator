// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"net"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/netbirdio/netbird/shared/management/http/api"
)

// childName is the deterministic name of a per-Service child object for one IP
// family, e.g. "app-ipv4" — unique within the Service's namespace.
func childName(svcName string, family corev1.IPFamily) string {
	return svcName + "-" + strings.ToLower(string(family))
}

// dnsRecordTTL is the TTL applied to the A/AAAA records the operator publishes.
const dnsRecordTTL = 5 * time.Minute

// serviceFQDN builds the single-label name under the zone for a Service:
// "<svc>-<ns>.<zoneDomain>". NetBird's managed zones only serve one label below
// the apex, so the service and namespace are hyphen-joined rather than nested.
func serviceFQDN(svcName, svcNamespace, zoneDomain string) string {
	return svcName + "-" + svcNamespace + "." + zoneDomain
}

// familyAddress pairs an IP address with its family.
type familyAddress struct {
	family  corev1.IPFamily
	address string
}

// ipFamilyOf classifies an IP string as IPv4 or IPv6, or "" when it is not a
// valid IP.
func ipFamilyOf(ip string) corev1.IPFamily {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if parsed.To4() != nil {
		return corev1.IPv4Protocol
	}
	return corev1.IPv6Protocol
}

// dnsRecordTypeFor classifies an IP string as an A (IPv4) or AAAA (IPv6) record.
// ok is false when the string is not a valid IP and should be skipped.
func dnsRecordTypeFor(ip string) (api.DNSRecordType, bool) {
	switch ipFamilyOf(ip) {
	case corev1.IPv4Protocol:
		return api.DNSRecordTypeA, true
	case corev1.IPv6Protocol:
		return api.DNSRecordTypeAAAA, true
	default:
		return "", false
	}
}

// recordMatchKey builds a comparison key for a DNS record that is stable across
// the multiple textual forms of an IP. An IPv6 address has several
// representations (e.g. "2001:db8::1" vs "2001:0db8:0:0:0:0:0:1"); comparing the
// canonicalized IP avoids deleting-and-recreating a record that already matches
// (which hits NetBird's "identical record already exists").
func recordMatchKey(recordType, content string) string {
	if ip := net.ParseIP(content); ip != nil {
		content = ip.String()
	}
	return recordType + "|" + content
}
