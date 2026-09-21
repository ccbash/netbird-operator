// SPDX-License-Identifier: BSD-3-Clause

package version

import (
	"runtime/debug"
)

const (
	NetbirdClientImage = "ghcr.io/netbirdio/netbird:0.77.0@sha256:fcf1d37d1a9b8c65e2968193d8762896750cfb4e81b04862b1b464f4eede3b9e"
	KubeApiProxyImage  = "ghcr.io/netbirdio/netbird-kubeapi-proxy:v0.0.5@sha256:af3c0b747d1ef307556ade3d1b59cf79cea6deac01f5c9d4ddd48f4e1154599a"
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
