// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

// errDependencyNotReady marks a transient condition where a referenced object
// (a Network, DNSZone, proxy cluster, ...) is not yet resolvable. The mirror
// reconciler surfaces it as a non-Ready condition and requeues, instead of
// erroring every reconcile.
var errDependencyNotReady = errors.New("dependency not ready")

// mirrorObject is a NetBird-mirror CRD: a client.Object that carries status
// conditions.
type mirrorObject interface {
	client.Object
	conditions.Setter
}

// mirror holds the per-kind behavior the generic reconciler drives. apply
// upserts the object to NetBird and records its id(s) in status; del removes it
// using those id(s). Everything else — finalizer, conditions, requeue, patching
// — is shared by MirrorReconciler.
type mirror[T mirrorObject] struct {
	kind      string
	finalizer string
	newObject func() T
	apply     func(ctx context.Context, nb *netbird.Client, c client.Client, obj T) error
	del       func(ctx context.Context, nb *netbird.Client, obj T) error
	// inUseMsg is logged (and emitted as an event) when a delete is rejected
	// because the NetBird object is still referenced; the delete is retried.
	inUseMsg string
}

// MirrorReconciler reconciles a single NetBird-mirror CRD kind by applying its
// spec straight to the NetBird API. One implementation, registered per kind —
// see NewNetworkReconciler, NewNetworkResourceReconciler, etc.
type MirrorReconciler[T mirrorObject] struct {
	client.Client
	Netbird  *netbird.Client
	Recorder record.EventRecorder
	m        mirror[T]
}

func (r *MirrorReconciler[T]) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	obj := r.m.newObject()
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sp := patch.NewSerialPatcher(obj, r.Client)
	logf.FromContext(ctx).V(1).Info("reconciling " + r.m.kind)

	if !obj.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, sp, obj)
	}

	controllerutil.AddFinalizer(obj, k8sutil.Finalizer(r.m.finalizer))
	if err := sp.Patch(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.m.apply(ctx, r.Netbird, r.Client, obj); err != nil {
		if errors.Is(err, errDependencyNotReady) {
			conditions.MarkFalse(obj, nbv1alpha1.ReadyCondition, nbv1alpha1.DependencyReason, "%s", err.Error())
			if perr := sp.Patch(ctx, obj); perr != nil {
				return ctrl.Result{}, perr
			}
			return ctrl.Result{RequeueAfter: dependencyRetry}, nil
		}
		return ctrl.Result{}, err
	}

	conditions.MarkTrue(obj, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	if err := sp.Patch(ctx, obj, patch.WithStatusObservedGeneration{}); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *MirrorReconciler[T]) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, obj T) (ctrl.Result, error) {
	err := r.m.del(ctx, r.Netbird, obj)
	switch {
	case err == nil, netbird.IsNotFound(err):
		// deleted, or already gone
	case netbirdutil.IsConflict(err):
		msg := r.m.inUseMsg
		if msg == "" {
			msg = "NetBird " + r.m.kind + " still in use; retrying deletion"
		}
		logf.FromContext(ctx).Info(msg)
		recordEvent(r.Recorder, obj, reasonInUse, "%s", msg)
		return ctrl.Result{RequeueAfter: cleanupRetry}, nil
	default:
		return ctrl.Result{}, err
	}

	controllerutil.RemoveFinalizer(obj, k8sutil.Finalizer(r.m.finalizer))
	if err := sp.Patch(ctx, obj); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *MirrorReconciler[T]) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(r.m.newObject()).
		WithLogConstructor(logConstructor(mgr, r.m.kind)).
		Complete(r)
}

// resolveNetworkID reads the NetBird network id from a referenced Network,
// returning errDependencyNotReady while the Network is missing or not yet
// reconciled.
func resolveNetworkID(ctx context.Context, c client.Client, ref nbv1alpha1.CrossNamespaceReference) (string, error) {
	network := &nbv1alpha1.Network{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, network)
	if kerrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: Network %s/%s not found", errDependencyNotReady, ref.Namespace, ref.Name)
	}
	if err != nil {
		return "", err
	}
	if network.Status.NetworkID == "" {
		return "", fmt.Errorf("%w: Network %s/%s has no network id yet", errDependencyNotReady, ref.Namespace, ref.Name)
	}
	return network.Status.NetworkID, nil
}

// resolveZoneID reads the NetBird zone id from a referenced DNSZone, returning
// errDependencyNotReady while the DNSZone is missing or not yet reconciled.
func resolveZoneID(ctx context.Context, c client.Client, ref nbv1alpha1.CrossNamespaceReference) (string, error) {
	zone := &nbv1alpha1.DNSZone{}
	err := c.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, zone)
	if kerrors.IsNotFound(err) {
		return "", fmt.Errorf("%w: DNSZone %s/%s not found", errDependencyNotReady, ref.Namespace, ref.Name)
	}
	if err != nil {
		return "", err
	}
	if zone.Status.ZoneID == "" {
		return "", fmt.Errorf("%w: DNSZone %s/%s has no zone id yet", errDependencyNotReady, ref.Namespace, ref.Name)
	}
	return zone.Status.ZoneID, nil
}
