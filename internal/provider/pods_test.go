package provider

import (
	"context"
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

func poolPod(name, instance, pool string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		Labels: map[string]string{labelInstance: instance, labelPool: pool},
	}}
}

func TestLabelPods(t *testing.T) {
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: {},
				common.ComponentRead:   {Replicas: ptr.To[int32](1)},
			},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, neo4jv1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		instance,
		poolPod("db-primary-0", "db", "primary"),
		poolPod("db-read-0", "db", "read"),
		poolPod("other-primary-0", "other", "primary"),
	).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)

	require.NoError(t, labelPods(c))

	for pod, component := range map[string]string{"db-primary-0": common.ComponentEngine, "db-read-0": common.ComponentRead} {
		got := &corev1.Pod{}
		require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: pod}, got))
		assert.Equal(t, "db", got.Labels[controller.InstanceLabel], pod)
		assert.Equal(t, component, got.Labels[controller.ComponentLabel], pod)
		assert.NotEmpty(t, got.Labels[labelPool], "the operator's labels are kept on %s", pod)
	}
	other := &corev1.Pod{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: "other-primary-0"}, other))
	assert.NotContains(t, other.Labels, controller.InstanceLabel)
	assert.ElementsMatch(t, []string{common.ComponentEngine, common.ComponentRead}, c.LabelledComponents())
}
