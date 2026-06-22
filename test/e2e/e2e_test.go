package e2e

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-openapi/testify/v2/require"
	"github.com/moby/moby/client"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/loader"
	"helm.sh/helm/v4/pkg/kube"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	kindv1alpha1 "sigs.k8s.io/kind/pkg/apis/config/v1alpha4"
	"sigs.k8s.io/kind/pkg/cluster"
	"sigs.k8s.io/kind/pkg/cluster/nodeutils"
)

const (
	netbirdNamespace = "netbird"
)

func TestE2E(t *testing.T) {
	imgRef := os.Getenv("IMG_REF")
	require.NotEmpty(t, imgRef)

	mobyClient, err := client.New(client.FromEnv)
	require.NoError(t, err)
	t.Cleanup(func() {
		err = mobyClient.Close()
		if err != nil {
			t.Log("could not close moby client", err)
		}
	})

	t.Log("Exporting image", imgRef)
	saveRes, err := mobyClient.ImageSave(t.Context(), []string{imgRef})
	require.NoError(t, err)
	imgPath := filepath.Join(t.TempDir(), "image")
	f, err := os.OpenFile(imgPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	_, err = io.Copy(f, saveRes)
	require.NoError(t, err)
	err = f.Close()
	require.NoError(t, err)

	kubernetesVersions := []string{
		"1.36.0",
	}
	for _, kubernetesVersion := range kubernetesVersions {
		t.Run(kubernetesVersion, func(t *testing.T) {
			t.Log("Creating Kind cluster")
			kcPath := filepath.Join(t.TempDir(), "kind.kubeconfig")
			provider := cluster.NewProvider()
			createCfg := &kindv1alpha1.Cluster{
				Nodes: []kindv1alpha1.Node{
					{
						Role: kindv1alpha1.ControlPlaneRole,
					},
				},
			}
			createOpts := []cluster.CreateOption{
				cluster.CreateWithV1Alpha4Config(createCfg),
				cluster.CreateWithNodeImage(fmt.Sprintf("ghcr.io/spegel-org/test-images/kind-node:%s", kubernetesVersion)),
				cluster.CreateWithKubeconfigPath(kcPath),
			}
			kindName := fmt.Sprintf("netbird-operator-e2e-%s", strings.ReplaceAll(kubernetesVersion, ".", "-"))
			err = provider.Create(kindName, createOpts...)
			require.NoError(t, err)
			t.Cleanup(func() {
				if t.Failed() {
					return
				}
				err = provider.Delete(kindName, "")
				require.NoError(t, err)
			})
			kindNodes, err := provider.ListNodes(kindName)
			require.NoError(t, err)

			k8sCfg, err := clientcmd.BuildConfigFromFlags("", kcPath)
			require.NoError(t, err)
			k8sClient, err := kubernetes.NewForConfig(k8sCfg)
			require.NoError(t, err)

			require.Eventually(t, func(ctx context.Context) error {
				for _, kindNode := range kindNodes {
					node, err := k8sClient.CoreV1().Nodes().Get(ctx, kindNode.String(), metav1.GetOptions{})
					if err != nil {
						return err
					}
					idx := slices.IndexFunc(node.Status.Conditions, func(cond corev1.NodeCondition) bool {
						return cond.Type == corev1.NodeReady
					})
					if idx == -1 {
						return errors.New("ready condition not found")
					}
					if node.Status.Conditions[idx].Status != corev1.ConditionTrue {
						return fmt.Errorf("node %s is not ready", kindNode.String())
					}
				}
				return nil
			}, 30*time.Second, 1*time.Second)

			t.Log("Importing image", imgRef)
			f, err := os.Open(imgPath)
			require.NoError(t, err)
			for _, node := range kindNodes {
				_, err = f.Seek(0, io.SeekStart)
				require.NoError(t, err)
				err = nodeutils.LoadImageArchive(node, f)
				require.NoError(t, err)
			}

			namespace := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: netbirdNamespace,
				},
			}
			_, err = k8sClient.CoreV1().Namespaces().Create(t.Context(), namespace, metav1.CreateOptions{})
			require.NoError(t, err)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "netbird-mgmt-api-key",
					Namespace: netbirdNamespace,
				},
				StringData: map[string]string{
					"NB_API_KEY": "dummy",
				},
			}
			_, err = k8sClient.CoreV1().Secrets(netbirdNamespace).Create(t.Context(), secret, metav1.CreateOptions{})
			require.NoError(t, err)
			installOperator(t, kcPath, imgRef)

			// The chart installs the operator's CRDs and the API server serves
			// them (a failure here means the chart didn't ship a CRD or the
			// operator couldn't register its controller for it).
			rl, err := k8sClient.Discovery().ServerResourcesForGroupVersion("netbird.io/v1alpha1")
			require.NoError(t, err)
			served := map[string]bool{}
			for _, r := range rl.APIResources {
				served[r.Name] = true
			}
			for _, want := range []string{
				"networks", "networkrouters", "networkresources",
				"dnszones", "dnsrecords", "reverseproxyservices",
				"groups", "setupkeys",
			} {
				require.True(t, served[want], "CRD %q should be installed by the chart", want)
			}
		})
	}
}

// installOperator installs the local (dev) chart with the freshly built image
// loaded into Kind. (The upstream netbirdio chart is a different, divergent
// operator with incompatible CRDs, so it's not part of this fork's e2e.)
func installOperator(t *testing.T, kcPath, imgRef string) {
	t.Helper()

	actionCfg := &action.Configuration{}
	actionCfg.SetLogger(slog.DiscardHandler)
	clientGetter := &genericclioptions.ConfigFlags{KubeConfig: &kcPath, Namespace: new(netbirdNamespace)}
	require.NoError(t, actionCfg.Init(clientGetter, netbirdNamespace, "secret"))

	charter, err := loader.Load("../../charts/netbird-operator")
	require.NoError(t, err)

	imgRegistry, repository, tag := splitImageRef(imgRef)
	t.Log("Deploying NetBird Operator", imgRef)
	vals := map[string]any{
		"webhook": map[string]any{
			"enableCertManager": false,
			"failurePolicy":     "Ignore",
		},
		"operator": map[string]any{
			"image": map[string]any{
				"registry":   imgRegistry,
				"repository": repository,
				"tag":        tag,
				"pullPolicy": "Never",
			},
		},
	}

	install := action.NewInstall(actionCfg)
	install.ReleaseName = "netbird-operator"
	install.Namespace = netbirdNamespace
	install.CreateNamespace = true
	install.WaitStrategy = kube.StatusWatcherStrategy
	install.Timeout = 90 * time.Second
	_, err = install.RunWithContext(t.Context(), charter, vals)
	require.NoError(t, err)
}

// splitImageRef splits "registry/repository:tag" into its parts.
func splitImageRef(ref string) (registry, repository, tag string) {
	tag = "latest"
	if i := strings.LastIndex(ref, ":"); i != -1 && !strings.Contains(ref[i:], "/") {
		tag = ref[i+1:]
		ref = ref[:i]
	}
	if i := strings.Index(ref, "/"); i != -1 {
		registry = ref[:i]
		repository = ref[i+1:]
	} else {
		repository = ref
	}
	return registry, repository, tag
}
