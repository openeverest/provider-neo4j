package provider

import (
	"context"
	"testing"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

func TestProviderUpgradeWaitsForMaintenance(t *testing.T) {
	t.Parallel()
	release := providerSpecFixture(t).Spec.Release.Version
	require.NotEmpty(t, release)
	token := upgradeTokenPrefix + release

	newClient := func(t *testing.T, in *corev1alpha1.Instance) client.Client {
		t.Helper()
		scheme := runtime.NewScheme()
		for _, add := range []func(*runtime.Scheme) error{
			corev1.AddToScheme, appsv1.AddToScheme, cmapi.AddToScheme, neo4jv1.AddToScheme,
			corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme, monitoringv1alpha1.AddToScheme,
		} {
			require.NoError(t, add(scheme))
		}
		// The fake client persists dry-run applies; the API server does not.
		dryRunApply := func(ctx context.Context, cl client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			o := &client.ApplyOptions{}
			o.ApplyOptions(opts)
			if len(o.DryRun) > 0 {
				return nil
			}
			return cl.Apply(ctx, obj, opts...)
		}
		return fake.NewClientBuilder().WithScheme(scheme).WithObjects(providerSpecFixture(t), in).
			WithInterceptorFuncs(interceptor.Funcs{Apply: dryRunApply}).Build()
	}
	sync := func(t *testing.T, cl client.Client, in *corev1alpha1.Instance) *controller.Context {
		t.Helper()
		c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
		require.NoError(t, New().Sync(c))
		return c
	}
	live := func(t *testing.T, cl client.Client) *neo4jv1.Neo4j {
		t.Helper()
		out := &neo4jv1.Neo4j{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "graph"}, out))
		return out
	}
	// previousRelease applies what release 0.0.1 built: an HTTPS listener
	// without TLS, which later releases drop.
	previousRelease := func(t *testing.T, cl client.Client, in *corev1alpha1.Instance) {
		t.Helper()
		c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
		old, err := buildNeo4j(c)
		require.NoError(t, err)
		old.Spec.Connectivity.Listeners.HTTPS = int32Ptr(7473)
		old.Annotations = map[string]string{releaseAnnotation: "0.0.1"}
		require.NoError(t, c.Apply(old))
	}
	standalone := func(t *testing.T, maintenance *corev1alpha1.MaintenanceSpec) *corev1alpha1.Instance {
		t.Helper()
		in := tlsInstance(t, common.TopologyStandalone, map[string]any{"mode": "disabled"})
		in.Spec.Maintenance = maintenance
		return in
	}

	t.Run("new instance applies", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, nil)
		cl := newClient(t, in)
		c := sync(t, cl, in)
		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, release, live(t, cl).Annotations[releaseAnnotation])
	})
	t.Run("changed rendering is held", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, nil)
		cl := newClient(t, in)
		previousRelease(t, cl, in)
		c := sync(t, cl, in)
		pending := c.GetPendingMaintenance()
		require.Len(t, pending, 1)
		assert.Equal(t, token, pending[0].ApprovalToken)
		assert.Equal(t, controller.MaintenanceDowntime, pending[0].Severity)
		got := live(t, cl)
		assert.Equal(t, int32Ptr(7473), got.Spec.Connectivity.Listeners.HTTPS)
		assert.Equal(t, "0.0.1", got.Annotations[releaseAnnotation])
	})
	t.Run("approval applies", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, &corev1alpha1.MaintenanceSpec{Approved: token})
		cl := newClient(t, in)
		previousRelease(t, cl, in)
		c := sync(t, cl, in)
		assert.Empty(t, c.GetPendingMaintenance())
		got := live(t, cl)
		assert.Nil(t, got.Spec.Connectivity.Listeners.HTTPS)
		assert.Equal(t, release, got.Annotations[releaseAnnotation])
	})
	t.Run("standing tolerance applies", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, &corev1alpha1.MaintenanceSpec{AutoApproveUpTo: corev1alpha1.MaintenanceDowntime})
		cl := newClient(t, in)
		previousRelease(t, cl, in)
		c := sync(t, cl, in)
		assert.Empty(t, c.GetPendingMaintenance())
		assert.Nil(t, live(t, cl).Spec.Connectivity.Listeners.HTTPS)
	})
	t.Run("unchanged rendering applies", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, nil)
		cl := newClient(t, in)
		c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
		old, err := buildNeo4j(c)
		require.NoError(t, err)
		require.NoError(t, c.Apply(old))
		c = sync(t, cl, in)
		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, release, live(t, cl).Annotations[releaseAnnotation])
	})
	t.Run("owner edit within a release applies", func(t *testing.T) {
		t.Parallel()
		in := standalone(t, nil)
		cl := newClient(t, in)
		sync(t, cl, in)
		in.Spec.Components[common.ComponentEngine].Resources.Limits[corev1.ResourceCPU] = resource.MustParse("2")
		c := sync(t, cl, in)
		assert.Empty(t, c.GetPendingMaintenance())
		assert.Equal(t, "2", live(t, cl).Spec.Resources.Limits.Cpu().String())
	})
	t.Run("cluster restarts one member at a time", func(t *testing.T) {
		t.Parallel()
		in := tlsInstance(t, common.TopologyCluster, map[string]any{"issuer": map[string]any{"name": "company-ca"}})
		cl := newClient(t, in)
		c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
		old, err := buildNeo4j(c)
		require.NoError(t, err)
		old.Spec.Config.JVM.AdditionalArguments = []string{"-XX:+UseG1GC"}
		require.NoError(t, c.Apply(old))
		c = sync(t, cl, in)
		pending := c.GetPendingMaintenance()
		require.Len(t, pending, 1)
		assert.Equal(t, controller.MaintenanceRollingRestart, pending[0].Severity)
		got := live(t, cl)
		require.NotNil(t, got.Spec.Config.JVM)
		assert.Equal(t, []string{"-XX:+UseG1GC"}, got.Spec.Config.JVM.AdditionalArguments)
	})
}
