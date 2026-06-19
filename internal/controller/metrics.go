// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// staleNetworkResources reports, per NetworkResource, how many old NetBird
// resources are still draining after a routing-mode change (i.e. still targeted
// by a reverse-proxy). A value that stays above zero indicates a proxy that
// never repointed — useful to alert on.
var staleNetworkResources = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "netbird_operator_networkresource_stale_resources",
		Help: "Number of NetBird resources awaiting deletion after a routing-mode change, per NetworkResource.",
	},
	[]string{"namespace", "name"},
)

func init() {
	metrics.Registry.MustRegister(staleNetworkResources)
}
