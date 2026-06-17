// SPDX-License-Identifier: BSD-3-Clause

package v1

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	nbv1alpha1 "github.com/netbirdio/kubernetes-operator/api/v1alpha1"
	"github.com/netbirdio/kubernetes-operator/internal/controller"
)

const SidecarProfileAnnotation = "netbird.io/sidecar-profile"

// SetupPodWebhookWithManager registers the webhook for Pod in the manager.
func SetupPodWebhookWithManager(mgr ctrl.Manager, managementURL, clientImage string) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(&PodNetbirdInjector{
			client:        mgr.GetClient(),
			managementURL: managementURL,
			clientImage:   clientImage,
		}).
		Complete()
}

// PodNetbirdInjector struct is responsible for setting default values on the custom resource of the
// Kind Pod when those are created or updated.
type PodNetbirdInjector struct {
	client        client.Client
	managementURL string
	clientImage   string
}

var _ admission.Defaulter[*corev1.Pod] = &PodNetbirdInjector{}

func (d *PodNetbirdInjector) Default(ctx context.Context, pod *corev1.Pod) error {
	// Find sidecar profiles matching pods labels.
	sidecarProfileList := &nbv1alpha1.SidecarProfileList{}
	err := d.client.List(ctx, sidecarProfileList, client.InNamespace(pod.Namespace))
	if err != nil {
		return err
	}
	sidecarProfiles := []nbv1alpha1.SidecarProfile{}
	for _, sidecarProfile := range sidecarProfileList.Items {
		if sidecarProfile.Spec.PodSelector == nil || sidecarProfile.Spec.PodSelector.Size() == 0 {
			sidecarProfiles = append(sidecarProfiles, sidecarProfile)
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(sidecarProfile.Spec.PodSelector)
		if err != nil {
			return err
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			sidecarProfiles = append(sidecarProfiles, sidecarProfile)
		}
	}
	// Do nothing if no profile matches.
	if len(sidecarProfiles) == 0 {
		return nil
	}
	// If two match we chose the first in alphabetical order.
	if len(sidecarProfiles) > 1 {
		slices.SortFunc(sidecarProfiles, func(a, b nbv1alpha1.SidecarProfile) int {
			return cmp.Compare(a.Name, b.Name)
		})
	}
	sidecarProfile := sidecarProfiles[0]

	// Get setup key referenced by sidecar profile.
	setupKey := &nbv1alpha1.SetupKey{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sidecarProfile.Spec.SetupKeyRef.Name,
			Namespace: pod.Namespace,
		},
	}
	err = d.client.Get(ctx, client.ObjectKeyFromObject(setupKey), setupKey)
	if err != nil {
		return err
	}

	// Add sidecar container.
	envVars := []corev1.EnvVar{
		{
			Name: "NB_SETUP_KEY",
			ValueFrom: &corev1.EnvVarSource{
				SecretKeyRef: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: setupKey.SecretName(),
					},
					Key: controller.SetupKeySecretKey,
				},
			},
		},
		{
			Name:  "NB_MANAGEMENT_URL",
			Value: d.managementURL,
		},
	}
	if len(sidecarProfile.Spec.ExtraDNSLabels) > 0 {
		envVars = append(envVars, corev1.EnvVar{
			Name:  "NB_EXTRA_DNS_LABELS",
			Value: strings.Join(sidecarProfile.Spec.ExtraDNSLabels, ","),
		})
	}

	container := corev1.Container{
		Name:  "netbird",
		Image: d.clientImage,
		Env:   envVars,
		SecurityContext: &corev1.SecurityContext{
			Capabilities: &corev1.Capabilities{
				Add: []corev1.Capability{"NET_ADMIN"},
			},
		},
	}
	if sidecarProfile.Spec.ContainerOverride != nil {
		baseJSON, err := json.Marshal(&container)
		if err != nil {
			return err
		}
		overrideJSON, err := json.Marshal(sidecarProfile.Spec.ContainerOverride)
		if err != nil {
			return err
		}
		mergedJSON, err := strategicpatch.StrategicMergePatch(baseJSON, overrideJSON, corev1.Container{})
		if err != nil {
			return err
		}
		err = json.Unmarshal(mergedJSON, &container)
		if err != nil {
			return err
		}
	}

	switch sidecarProfile.Spec.InjectionMode {
	case nbv1alpha1.InjectionModeSidecar:
		restartPolicy := corev1.ContainerRestartPolicyAlways
		container.RestartPolicy = &restartPolicy
		pod.Spec.InitContainers = append(pod.Spec.InitContainers, container)
	case nbv1alpha1.InjectionModeContainer:
		pod.Spec.Containers = append(pod.Spec.Containers, container)
	default:
		return fmt.Errorf("unknown injection mode %s", sidecarProfile.Spec.InjectionMode)
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[SidecarProfileAnnotation] = sidecarProfile.Name

	return nil
}
