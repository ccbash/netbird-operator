// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=reverseproxyservices/finalizers,verbs=update

// NewReverseProxyServiceReconciler builds the reconciler for the
// ReverseProxyService CRD. It reuses the generic mirror reconciler — its apply
// closure targets each backend LoadBalancer Service's DNSRecord FQDN.
func NewReverseProxyServiceReconciler(c client.Client, nb *netbird.Client, rec record.EventRecorder) *MirrorReconciler[*nbv1alpha1.ReverseProxyService] {
	return &MirrorReconciler[*nbv1alpha1.ReverseProxyService]{
		Client:   c,
		Netbird:  nb,
		Recorder: rec,
		m: mirror[*nbv1alpha1.ReverseProxyService]{
			kind:      "ReverseProxyService",
			finalizer: "reverseproxyservice",
			newObject: func() *nbv1alpha1.ReverseProxyService { return &nbv1alpha1.ReverseProxyService{} },
			apply:     applyReverseProxyService,
			del:       deleteReverseProxyService,
		},
	}
}

func applyReverseProxyService(ctx context.Context, nb *netbird.Client, c client.Client, rps *nbv1alpha1.ReverseProxyService) error {
	cluster, err := netbirdutil.GetProxyClusterByAddress(ctx, nb, rps.Spec.ProxyCluster)
	if errors.Is(err, netbirdutil.ErrProxyClusterNotFound) {
		return fmt.Errorf("%w: proxy cluster %s not found", errDependencyNotReady, rps.Spec.ProxyCluster)
	}
	if err != nil {
		return err
	}

	targets := make([]api.ServiceTarget, 0, len(rps.Spec.Backends))
	for _, b := range rps.Spec.Backends {
		fqdn, err := backendFQDN(ctx, c, rps.Namespace, b.ServiceRef.Name)
		if err != nil {
			return err
		}
		port, err := backendPort(ctx, c, rps.Namespace, b.ServiceRef.Name, b.Port)
		if err != nil {
			return err
		}
		host := fqdn
		direct := true
		target := api.ServiceTarget{
			Enabled:    true,
			Host:       &host,
			Port:       port,
			Protocol:   api.ServiceTargetProtocolHttp,
			TargetType: api.ServiceTargetTargetTypeCluster,
			TargetId:   cluster.Id,
			Options:    &api.ServiceTargetOptions{DirectUpstream: &direct},
		}
		if b.Path != "" {
			path := b.Path
			target.Path = &path
		}
		targets = append(targets, target)
	}
	sortServiceTargets(targets)

	accessGroups, err := netbirdutil.GetGroupIDs(ctx, c, nb, rps.Spec.AccessGroups, rps.Namespace)
	if err != nil {
		return err
	}

	mode := api.ServiceRequestModeHttp
	req := api.ServiceRequest{
		Domain:           rps.Spec.Domain,
		Enabled:          true,
		Mode:             &mode,
		Name:             rps.Name,
		Targets:          &targets,
		Private:          rps.Spec.Private,
		PassHostHeader:   rps.Spec.PassHostHeader,
		RewriteRedirects: rps.Spec.RewriteRedirects,
	}
	if len(accessGroups) > 0 {
		req.AccessGroups = &accessGroups
	}
	if ar := accessRestrictionsFor(rps.Spec.CrowdsecMode, rps.Spec.AccessRestrictions); ar != nil {
		req.AccessRestrictions = ar
	}

	if rps.Status.ServiceID != "" {
		resp, err := nb.ReverseProxyServices.Update(ctx, rps.Status.ServiceID, req)
		if err == nil {
			rps.Status.ServiceID = resp.Id
			return nil
		}
		if !netbird.IsNotFound(err) {
			return err
		}
	}
	resp, err := nb.ReverseProxyServices.Create(ctx, req)
	if err != nil {
		return err
	}
	rps.Status.ServiceID = resp.Id
	return nil
}

func deleteReverseProxyService(ctx context.Context, nb *netbird.Client, rps *nbv1alpha1.ReverseProxyService) error {
	if rps.Status.ServiceID == "" {
		return nil
	}
	return nb.ReverseProxyServices.Delete(ctx, rps.Status.ServiceID)
}

// backendFQDN returns the dualstack DNS name advertised for a LoadBalancer
// Service (the DNSRecord the LoadBalancer controller published), or
// errDependencyNotReady while the Service is not yet advertised.
func backendFQDN(ctx context.Context, c client.Client, namespace, svcName string) (string, error) {
	var records nbv1alpha1.DNSRecordList
	if err := c.List(ctx, &records, client.InNamespace(namespace), client.MatchingLabels{lbServiceLabel: svcName}); err != nil {
		return "", err
	}
	if len(records.Items) == 0 {
		return "", fmt.Errorf("%w: Service %s/%s not advertised (no DNSRecord)", errDependencyNotReady, namespace, svcName)
	}
	return records.Items[0].Spec.Name, nil
}

// backendPort returns want, or the Service's first port when want is 0.
func backendPort(ctx context.Context, c client.Client, namespace, svcName string, want int) (int, error) {
	if want != 0 {
		return want, nil
	}
	svc := &corev1.Service{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: svcName}, svc); err != nil {
		return 0, err
	}
	if len(svc.Spec.Ports) == 0 {
		return 0, fmt.Errorf("%w: Service %s/%s has no ports", errDependencyNotReady, namespace, svcName)
	}
	return int(svc.Spec.Ports[0].Port), nil
}

// sortServiceTargets orders targets deterministically so an unchanged reconcile
// renders an identical ServiceRequest.
func sortServiceTargets(targets []api.ServiceTarget) {
	sort.Slice(targets, func(i, j int) bool {
		hi, hj := "", ""
		if targets[i].Host != nil {
			hi = *targets[i].Host
		}
		if targets[j].Host != nil {
			hj = *targets[j].Host
		}
		if hi != hj {
			return hi < hj
		}
		return targets[i].Port < targets[j].Port
	})
}

// accessRestrictionsFor maps the CRD's restriction fields onto the NetBird API
// type, or returns nil when none are set.
func accessRestrictionsFor(mode *nbv1alpha1.CrowdsecMode, ar *nbv1alpha1.AccessRestrictions) *api.AccessRestrictions {
	if mode == nil && ar == nil {
		return nil
	}
	var out api.AccessRestrictions
	if mode != nil {
		m := api.AccessRestrictionsCrowdsecMode(*mode)
		out.CrowdsecMode = &m
	}
	if ar != nil {
		if len(ar.AllowedCidrs) > 0 {
			v := append([]string(nil), ar.AllowedCidrs...)
			out.AllowedCidrs = &v
		}
		if len(ar.BlockedCidrs) > 0 {
			v := append([]string(nil), ar.BlockedCidrs...)
			out.BlockedCidrs = &v
		}
		if len(ar.AllowedCountries) > 0 {
			v := append([]string(nil), ar.AllowedCountries...)
			out.AllowedCountries = &v
		}
		if len(ar.BlockedCountries) > 0 {
			v := append([]string(nil), ar.BlockedCountries...)
			out.BlockedCountries = &v
		}
	}
	return &out
}
