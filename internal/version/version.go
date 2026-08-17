// SPDX-License-Identifier: BSD-3-Clause

package version

import (
	"runtime/debug"
)

const (
	NetbirdClientImage = "ghcr.io/netbirdio/netbird:0.77.0@sha256:fcf1d37d1a9b8c65e2968193d8762896750cfb4e81b04862b1b464f4eede3b9e"
	KubeApiProxyImage  = "ghcr.io/netbirdio/netbird-kubeapi-proxy:v0.0.4@sha256:bffa4f093abc19b4934ae37657bac76fa3b390cbd39aadac987634215eb750f5"
	// ReverseProxyImage is the NetBird bring-your-own reverse-proxy image the
	// ReverseProxyCluster controller deploys. Pinned to :latest for now — pin a
	// version+digest matching the netbird module once a tag is confirmed.
	ReverseProxyImage = "netbirdio/reverse-proxy:latest"
)

func BuildVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	modified := true
	for _, s := range bi.Settings {
		if s.Key == "vcs.modified" {
			if s.Value == "false" {
				modified = false
			}
			break
		}
	}

	develVersion := "devel"
	if modified {
		return develVersion
	}
	if bi.Main.Version == "" {
		return develVersion
	}
	return bi.Main.Version
}
