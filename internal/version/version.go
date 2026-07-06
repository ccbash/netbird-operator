// SPDX-License-Identifier: BSD-3-Clause

package version

import (
	"runtime/debug"
)

const (
	NetbirdClientImage = "ghcr.io/netbirdio/netbird:0.74.2@sha256:f2feb70b5978b0773ea64ec3fb5061ab894cc1720264dfc5bf187cb336bfa9fd"
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
