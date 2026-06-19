// SPDX-License-Identifier: BSD-3-Clause

package controller

import (
	"context"

	"github.com/fluxcd/pkg/runtime/conditions"
	"github.com/fluxcd/pkg/runtime/patch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	netbird "github.com/netbirdio/netbird/shared/management/client/rest"
	"github.com/netbirdio/netbird/shared/management/http/api"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/k8sutil"
	"github.com/netbirdio/kubernetes-operator/internal/netbirdutil"
)

type GroupReconciler struct {
	client.Client

	Netbird  *netbird.Client
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=netbird.io,resources=groups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=netbird.io,resources=groups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=netbird.io,resources=groups/finalizers,verbs=update

func (r *GroupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	group := &nbv1alpha1.Group{}
	err := r.Get(ctx, req.NamespacedName, group)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	sp := patch.NewSerialPatcher(group, r.Client)

	logf.FromContext(ctx).V(1).Info("reconciling group")

	if !group.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, sp, group)
	}

	controllerutil.AddFinalizer(group, k8sutil.Finalizer("group"))
	err = sp.Patch(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}

	groupID, err := func() (string, error) {
		if group.Status.GroupID != "" {
			groupResp, err := r.Netbird.Groups.Get(ctx, group.Status.GroupID)
			if err != nil && !netbird.IsNotFound(err) {
				return "", err
			}
			if err == nil {
				peers := []string{}
				for _, peer := range groupResp.Peers {
					peers = append(peers, peer.Id)
				}
				groupReq := api.GroupRequest{
					Name:      group.Spec.Name,
					Peers:     &peers,
					Resources: &groupResp.Resources,
				}
				resp, err := r.Netbird.Groups.Update(ctx, group.Status.GroupID, groupReq)
				if err != nil && !netbird.IsNotFound(err) {
					return "", err
				}
				if err == nil {
					return resp.Id, nil
				}
			}
		}
		groupReq := api.GroupRequest{
			Name: group.Spec.Name,
		}
		resp, err := r.Netbird.Groups.Create(ctx, groupReq)
		if err != nil {
			return "", err
		}
		return resp.Id, nil
	}()
	if err != nil {
		return ctrl.Result{}, err
	}
	group.Status.GroupID = groupID

	conditions.MarkTrue(group, nbv1alpha1.ReadyCondition, nbv1alpha1.ReconciledReason, "")
	err = sp.Patch(ctx, group, patch.WithStatusObservedGeneration{})
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: resyncInterval}, nil
}

func (r *GroupReconciler) reconcileDelete(ctx context.Context, sp *patch.SerialPatcher, group *nbv1alpha1.Group) (ctrl.Result, error) {
	if group.Status.GroupID != "" {
		err := r.Netbird.Groups.Delete(ctx, group.Status.GroupID)
		switch {
		case err == nil, netbird.IsNotFound(err):
			// deleted, or already gone
		case netbirdutil.IsConflict(err):
			// The group is still referenced (by a resource, policy, router peer
			// group or setup key). Back off and retry instead of erroring every
			// reconcile — the finalizer keeps the object until the group frees.
			logf.FromContext(ctx).Info("group still in use, retrying deletion", "groupID", group.Status.GroupID)
			recordEvent(r.Recorder, group, corev1.EventTypeWarning, reasonInUse,
				"NetBird group %s still in use; retrying deletion", group.Status.GroupID)
			return ctrl.Result{RequeueAfter: cleanupRetry}, nil
		default:
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(group, k8sutil.Finalizer("group"))
	err := sp.Patch(ctx, group)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *GroupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&nbv1alpha1.Group{}).
		WithLogConstructor(logConstructor(mgr, "Group")).
		Complete(r)
}
