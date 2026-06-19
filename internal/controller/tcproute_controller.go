// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwv1alpha2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/gatewayutil"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	nbv1alpha1ac "github.com/netbirdio/kubernetes-operator/pkg/applyconfigurations/api/v1alpha1"
)

type TCPRouteReconciler struct {
	client.Client

	Recorder record.EventRecorder
}

func (r *TCPRouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.LoggerFrom(ctx)

	tr := &gwv1alpha2.TCPRoute{}
	err := r.Get(ctx, req.NamespacedName, tr)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sp := patch.NewSerialPatcher(tr, r.Client)

	if !tr.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, tr)
	}

	for _, parent := range tr.Spec.ParentRefs {
		if err := r.reconcileParent(ctx, logger, sp, tr, parent); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// reconcileParent ensures a NetworkResource per backend Service for a single
// parent Gateway. Parents whose Gateway is missing or not yet programmed are
// skipped.
func (r *TCPRouteReconciler) reconcileParent(ctx context.Context, logger logr.Logger, sp *patch.SerialPatcher, tr *gwv1alpha2.TCPRoute, parent gwv1.ParentReference) error {
	gw, err := gatewayutil.GetParentGateway(ctx, r.Client, parent, tr.Namespace, GatewayControllerName)
	if err != nil {
		return err
	}
	if gw == nil {
		return nil
	}
	if !meta.IsStatusConditionTrue(gw.Status.Conditions, string(gwv1.GatewayConditionProgrammed)) {
		logger.Info("gateway is not ready", "name", gw.ObjectMeta.Name)
		recordEvent(r.Recorder, tr, corev1.EventTypeWarning, reasonDependencyNotReady,
			"Gateway %s is not programmed yet", gw.Name)
		return nil
	}
	netRouter, err := gatewayutil.GetGatewayNetworkRouter(ctx, r.Client, gw)
	if err != nil {
		return err
	}

	logger.V(1).Info("reconciling TCPRoute", "gateway", gw.Name)

	controllerutil.AddFinalizer(tr, k8sutil.Finalizer("tcproute"))
	if err := sp.Patch(ctx, tr); err != nil {
		return err
	}

	svcIdx, err := collectBackendServices(ctx, r.Client, tr.Namespace, r.backendServiceNames(tr), false)
	if err != nil {
		return err
	}
	return r.ensureNetworkResources(ctx, tr, netRouter, svcIdx)
}

func (r *TCPRouteReconciler) ensureNetworkResources(ctx context.Context, tr *gwv1alpha2.TCPRoute, netRouter *nbv1alpha1.NetworkRouter, svcIdx map[string]corev1.Service) error {
	for _, svc := range svcIdx {
		controllerRef, err := k8sutil.ControllerReference(&svc, r.Scheme())
		if err != nil {
			return err
		}
		controllerRef = controllerRef.WithBlockOwnerDeletion(false)
		ownerRef, err := k8sutil.OwnerReference(tr, r.Scheme())
		if err != nil {
			return err
		}
		netResourceAC := nbv1alpha1ac.NetworkResource(svc.Name, svc.Namespace).
			WithOwnerReferences(controllerRef, ownerRef).
			WithSpec(
				nbv1alpha1ac.NetworkResourceSpec().
					WithNetworkRouterRef(nbv1alpha1ac.CrossNamespaceReference().WithName(netRouter.Name).WithNamespace(netRouter.Namespace)).
					WithServiceRef(corev1.LocalObjectReference{Name: svc.Name}),
			)
		if err := r.Client.Apply(ctx, netResourceAC, client.ForceOwnership); err != nil {
			return err
		}
	}
	return nil
}

// backendServiceNames returns the names of every Service referenced by the
// route's backendRefs.
func (r *TCPRouteReconciler) backendServiceNames(tr *gwv1alpha2.TCPRoute) []string {
	var names []string
	for _, rule := range tr.Spec.Rules {
		for _, ref := range rule.BackendRefs {
			names = append(names, string(ref.Name))
		}
	}
	return names
}

func (r *TCPRouteReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, tr *gwv1alpha2.TCPRoute) (ctrl.Result, error) {
	for _, parent := range tr.Spec.ParentRefs {
		gw, err := gatewayutil.GetParentGateway(ctx, r.Client, parent, tr.Namespace, GatewayControllerName)
		if err != nil {
			return ctrl.Result{}, err
		}
		if gw == nil {
			continue
		}

		svcIdx, err := collectBackendServices(ctx, r.Client, tr.Namespace, r.backendServiceNames(tr), true)
		if err != nil {
			return ctrl.Result{}, err
		}
		for _, svc := range svcIdx {
			if err := detachNetworkResource(ctx, r.Client, r.Scheme(), tr, svc); err != nil {
				return ctrl.Result{}, err
			}
		}
	}

	controllerutil.RemoveFinalizer(tr, k8sutil.Finalizer("tcproute"))
	err := sp.Patch(ctx, tr)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *TCPRouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gwv1alpha2.TCPRoute{}).
		WithLogConstructor(logConstructor(mgr, "TCPRoute")).
		Complete(r)
}
