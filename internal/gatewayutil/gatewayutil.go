// SPDX-License-Identifier: BSD-3-Clause

package gatewayutil

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
)

func GetParentGateway(ctx context.Context, k8sClient client.Client, parent gwv1.ParentReference, namespace, controllerName string) (*gwv1.Gateway, error) {
	if parent.Namespace != nil {
		namespace = string(*parent.Namespace)
	}
	gw := &gwv1.Gateway{}
	err := k8sClient.Get(ctx, types.NamespacedName{Namespace: namespace, Name: string(parent.Name)}, gw)
	if err != nil {
		return nil, err
	}
	gwc := &gwv1.GatewayClass{}
	err = k8sClient.Get(ctx, client.ObjectKey{Name: string(gw.Spec.GatewayClassName)}, gwc)
	if err != nil {
		return nil, err
	}
	if string(gwc.Spec.ControllerName) != controllerName {
		return nil, nil
	}

	// TODO (phillebaba): Enforce allowed routes in gateway.

	return gw, nil
}

// GetGatewayNetwork resolves the Network a Gateway is linked to, via its single
// listener (protocol "gateway.netbird.io/Network", name = the Network's name).
func GetGatewayNetwork(ctx context.Context, k8sClient client.Client, gw *gwv1.Gateway) (*nbv1alpha1.Network, error) {
	networkName, err := GetNetworkName(gw.Spec.Listeners)
	if err != nil {
		return nil, err
	}
	network := &nbv1alpha1.Network{}
	err = k8sClient.Get(ctx, types.NamespacedName{Namespace: gw.Namespace, Name: networkName}, network)
	if err != nil {
		return nil, err
	}
	return network, nil
}

// GetNetworkName extracts the linked Network's name from a Gateway's listeners.
// The netbird Gateway uses a single listener whose protocol is
// "gateway.netbird.io/Network" and whose name is the Network resource name.
func GetNetworkName(listeners []gwv1.Listener) (string, error) {
	if len(listeners) > 1 {
		return "", errors.New("netbird Gateway only supports a single listener")
	}
	group, kind, ok := strings.Cut(string(listeners[0].Protocol), "/")
	if !ok {
		return "", fmt.Errorf("invalid protocol %s, expected gateway.netbird.io/Network", listeners[0].Protocol)
	}
	if group != "gateway.netbird.io" || kind != "Network" {
		return "", fmt.Errorf("invalid group %s and kind %s, expected gateway.netbird.io/Network", group, kind)
	}
	return string(listeners[0].Name), nil
}
