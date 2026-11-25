package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

func rawExtension(t *testing.T, value any) *runtime.RawExtension {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	return &runtime.RawExtension{Raw: data}
}

func providerSpecFixture(t *testing.T) *corev1alpha1.Provider {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean("../../charts/provider-neo4j/generated/provider-spec.yaml"))
	require.NoError(t, err)
	var wrapper struct {
		Spec corev1alpha1.ProviderSpec `json:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(data, &wrapper))
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: common.ProviderName},
		Spec:       wrapper.Spec,
	}
}

func testContext(t *testing.T, instance *corev1alpha1.Instance, objects ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, monitoringv1alpha1.AddToScheme(scheme))
	require.NoError(t, neo4jv1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(providerSpecFixture(t), instance).
		WithObjects(objects...).
		Build()
	return controller.NewContext(context.Background(), cl, instance, common.ProviderName)
}

func validEngine() corev1alpha1.ComponentSpec {
	replicas := int32(3)
	return corev1alpha1.ComponentSpec{
		Replicas: &replicas,
		Storage:  &corev1alpha1.Storage{Size: resource.MustParse("10Gi")},
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("1"),
				corev1.ResourceMemory: resource.MustParse("2Gi"),
			},
		},
	}
}

func TestBuildNeo4jEnterpriseCluster(t *testing.T) {
	t.Parallel()
	analyticsReplicas := int32(2)
	readReplicas := int32(1)
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Version:     "2026.07.1",
			Parameters: rawExtension(t, map[string]any{
				"edition":           "enterprise",
				"licenseAcceptance": "yes",
			}),
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: validEngine(),
				common.ComponentAnalytics: {
					Replicas: &analyticsReplicas,
				},
				common.ComponentRead: {
					Replicas: &readReplicas,
				},
			},
		},
	}
	c := testContext(t, instance)

	got, err := buildNeo4j(c)
	require.NoError(t, err)
	assert.Equal(t, neo4jv1.EditionEnterprise, got.Spec.Edition)
	require.NotNil(t, got.Spec.License)
	assert.Equal(t, neo4jv1.LicenseAcceptYes, got.Spec.License.Accept)
	assert.Equal(t, neo4jv1.TopologyModeCluster, got.Spec.Topology.Mode)
	assert.Equal(t, int32(3), got.Spec.Topology.Primaries.Members)
	require.NotNil(t, got.Spec.Topology.Secondaries)
	assert.Equal(t, int32(2), got.Spec.Topology.Secondaries.Analytics.Members)
	assert.Equal(t, int32(1), got.Spec.Topology.Secondaries.Read.Members)
	require.NotNil(t, got.Spec.Trust.CertManager)
	assert.Equal(t, "graph-selfsigned", got.Spec.Trust.CertManager.IssuerRef.Name)
	assert.Equal(t, "graph-cluster-tls", got.Spec.Trust.Certificates.Cluster.SecretName)
	assert.NotNil(t, got.Spec.Trust.Certificates.Cluster.TrustedCerts)
	assert.Nil(t, got.Spec.Trust.Reload)
}

func TestBuildNeo4jDefaultsToCommunityStandalone(t *testing.T) {
	t.Parallel()
	one := int32(1)
	engine := validEngine()
	engine.Replicas = &one
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyStandalone},
			Version:     "2026.07.1",
			Components:  map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
	got, err := buildNeo4j(testContext(t, instance))
	require.NoError(t, err)
	assert.Equal(t, neo4jv1.EditionCommunity, got.Spec.Edition)
	assert.Nil(t, got.Spec.License)
	assert.Equal(t, neo4jv1.TopologyModeStandalone, got.Spec.Topology.Mode)
	assert.Nil(t, got.Spec.Features, "no backup and no metrics render no features, not an empty object")
}

func TestLicenseAndTopologyValidation(t *testing.T) {
	t.Parallel()
	instance := &corev1alpha1.Instance{
		Spec: corev1alpha1.InstanceSpec{
			Topology: &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: validEngine(),
			},
		},
	}
	result := (&Provider{}).Validate(controller.ValidationInput{Instance: instance})
	require.NotEmpty(t, result.Errors)
	assert.Equal(t, "spec.parameters.edition", result.Errors[0].Field)

	instance.Spec.Parameters = rawExtension(t, map[string]any{
		"edition": "enterprise",
	})
	result = (&Provider{}).Validate(controller.ValidationInput{Instance: instance})
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "spec.parameters.licenseAcceptance", result.Errors[0].Field)
}

func TestBuildConnectivityLoadBalancerSourceRanges(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		service *corev1alpha1.Service
		want    []string
	}{
		"the operator refuses a LoadBalancer without ranges, so unset means open": {
			service: &corev1alpha1.Service{ServiceType: corev1.ServiceTypeLoadBalancer},
			want:    []string{"0.0.0.0/0"},
		},
		"ranges are passed on, bare addresses as single hosts": {
			service: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeLoadBalancer,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8", "192.0.2.7"},
				},
			},
			want: []string{"10.0.0.0/8", "192.0.2.7/32"},
		},
		"ranges left over from a LoadBalancer do not reach another Service type": {
			service: &corev1alpha1.Service{
				ServiceType: corev1.ServiceTypeNodePort,
				LoadBalancerService: &corev1alpha1.LoadBalancerService{
					SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8"},
				},
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			engine := validEngine()
			engine.Service = tc.service
			got := buildConnectivity(engine).Service
			assert.Equal(t, string(tc.service.ServiceType), string(got.Type))
			assert.Equal(t, tc.want, got.LoadBalancerSourceRanges)
		})
	}
}

func TestValidateRejectsInvalidSourceRange(t *testing.T) {
	t.Parallel()
	one := int32(1)
	engine := validEngine()
	engine.Replicas = &one
	engine.Service = &corev1alpha1.Service{
		ServiceType: corev1.ServiceTypeLoadBalancer,
		LoadBalancerService: &corev1alpha1.LoadBalancerService{
			SourceRanges: corev1alpha1.SourceRanges{"10.0.0.0/8", "office"},
		},
	}
	instance := &corev1alpha1.Instance{
		Spec: corev1alpha1.InstanceSpec{
			Topology:   &corev1alpha1.TopologySpec{Type: common.TopologyStandalone},
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
	result := (&Provider{}).Validate(controller.ValidationInput{Instance: instance})
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "spec.components[engine].service.loadBalancerService.sourceRanges[1]", result.Errors[0].Field)
}

func clusterInstance(t *testing.T) *corev1alpha1.Instance {
	t.Helper()
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Version:     "2026.07.1",
			Parameters:  rawExtension(t, map[string]any{"edition": "enterprise", "licenseAcceptance": "yes"}),
			Components:  map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: validEngine()},
		},
	}
}

func TestClusterPodDisruptionBudgetAllowsOneMemberAtATime(t *testing.T) {
	t.Parallel()
	instance := clusterInstance(t)
	got, err := buildNeo4j(testContext(t, instance))
	require.NoError(t, err)
	require.NotNil(t, got.Spec.PodDisruptionBudget)
	assert.True(t, got.Spec.PodDisruptionBudget.Enabled)
	assert.Equal(t, int32(2), got.Spec.PodDisruptionBudget.MinAvailable.IntVal)

	// The budget covers every pool: with a lower minimum a drain could take
	// two of the three primaries.
	two := int32(2)
	instance.Spec.Components[common.ComponentRead] = corev1alpha1.ComponentSpec{Replicas: &two}
	got, err = buildNeo4j(testContext(t, instance))
	require.NoError(t, err)
	assert.Equal(t, int32(4), got.Spec.PodDisruptionBudget.MinAvailable.IntVal)

	// A budget for one pod would block every drain.
	one := int32(1)
	engine := validEngine()
	engine.Replicas = &one
	instance.Spec.Components = map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine}
	got, err = buildNeo4j(testContext(t, instance))
	require.NoError(t, err)
	assert.Nil(t, got.Spec.PodDisruptionBudget)
}

func TestClusterSpreadsMembersByDefault(t *testing.T) {
	t.Parallel()
	preset := func(n *neo4jv1.Neo4j) neo4jv1.PodAntiAffinityPreset {
		if n.Spec.Scheduling == nil || n.Spec.Scheduling.Affinity == nil {
			return ""
		}
		return n.Spec.Scheduling.Affinity.PodAntiAffinity
	}

	t.Run("a cluster prefers a node per member", func(t *testing.T) {
		t.Parallel()
		got, err := buildNeo4j(testContext(t, clusterInstance(t)))
		require.NoError(t, err)
		assert.Equal(t, antiAffinitySoft, preset(got))
	})
	t.Run("the Instance's own affinity replaces it", func(t *testing.T) {
		t.Parallel()
		instance := clusterInstance(t)
		engine := validEngine()
		own := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{}}
		engine.SchedulingPolicy = &commonv1alpha1.SchedulingPolicy{Affinity: own}
		instance.Spec.Components[common.ComponentEngine] = engine
		got, err := buildNeo4j(testContext(t, instance))
		require.NoError(t, err)
		assert.Empty(t, preset(got))
		assert.Equal(t, own, got.Spec.Scheduling.Affinity.Custom)
	})
	t.Run("a standalone has nothing to spread", func(t *testing.T) {
		t.Parallel()
		one := int32(1)
		engine := validEngine()
		engine.Replicas = &one
		instance := clusterInstance(t)
		instance.Spec.Topology.Type = common.TopologyStandalone
		instance.Spec.Components[common.ComponentEngine] = engine
		got, err := buildNeo4j(testContext(t, instance))
		require.NoError(t, err)
		assert.Nil(t, got.Spec.Scheduling)
		assert.Nil(t, got.Spec.PodDisruptionBudget)
	})
}

// A storage entry the runtime holds must not be rendered: the Neo4j keeps the
// credentials it has, and never gets the empty security the CRD rejects.
func TestBuildNeo4jHoldsTheBackupCredentialsOfAHeldStorage(t *testing.T) {
	t.Parallel()
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Version:     "2026.07.1",
			Parameters: rawExtension(t, map[string]any{
				"edition":           "enterprise",
				"licenseAcceptance": "yes",
			}),
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: validEngine()},
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled:  true,
				ClassRef: commonv1alpha1.ObjectRef{Name: "neo4j-backup"},
				// The BackupStorage does not exist, so the runtime holds the entry.
				Storages: []corev1alpha1.InstanceBackupStorage{{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}}},
			},
		},
	}

	got, err := buildNeo4j(testContext(t, instance))
	require.NoError(t, err)
	assert.Nil(t, got.Spec.Security, "nothing to hold yet")

	live := &neo4jv1.Neo4j{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default"},
		Spec: neo4jv1.Neo4jSpec{Security: &neo4jv1.SecuritySpec{
			CloudIdentity: &neo4jv1.CloudIdentity{StaticKeySecret: "everest-neo4j-graph-s3"},
		}},
	}
	got, err = buildNeo4j(testContext(t, instance, live))
	require.NoError(t, err)
	require.NotNil(t, got.Spec.Security)
	assert.Equal(t, live.Spec.Security.CloudIdentity, got.Spec.Security.CloudIdentity)
}
