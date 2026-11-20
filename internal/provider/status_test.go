package provider

import (
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

const testVersion = "2026.07.1"

func runningNeo4j(mode neo4jv1.TopologyMode) *neo4jv1.Neo4j {
	n := &neo4jv1.Neo4j{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Generation: 2},
		Spec: neo4jv1.Neo4jSpec{
			Version:  testVersion,
			Topology: neo4jv1.TopologySpec{Mode: mode},
		},
		Status: neo4jv1.Neo4jStatus{
			Phase:              neo4jv1.Neo4jPhaseRunning,
			ObservedGeneration: 2,
			Version:            testVersion,
			Conditions: []metav1.Condition{
				{Type: neo4jConditionReady, Status: metav1.ConditionTrue, Reason: "AllMembersReady", ObservedGeneration: 2},
			},
		},
	}
	if mode == neo4jv1.TopologyModeCluster {
		n.Spec.Topology.Primaries = &neo4jv1.PrimariesSpec{Members: 3}
		n.Status.Conditions = append(n.Status.Conditions,
			metav1.Condition{Type: neo4jConditionClusterFormed, Status: metav1.ConditionTrue, Reason: "Formed"})
	}
	return n
}

func settledSet(name string, replicas int32) appsv1.StatefulSet {
	return appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 1,
			Replicas:           replicas,
			ReadyReplicas:      replicas,
			UpdatedReplicas:    replicas,
			CurrentRevision:    "r1",
			UpdateRevision:     "r1",
		},
	}
}

func setCond(n *neo4jv1.Neo4j, cond metav1.Condition) {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == cond.Type {
			n.Status.Conditions[i] = cond
			return
		}
	}
	n.Status.Conditions = append(n.Status.Conditions, cond)
}

func operationTypes(st controller.Status) []string {
	var out []string
	for _, op := range st.Operations {
		out = append(out, op.Type)
	}
	return out
}

func TestObserve(t *testing.T) {
	standalone := func(mutate func(*neo4jv1.Neo4j, *appsv1.StatefulSet)) observation {
		n := runningNeo4j(neo4jv1.TopologyModeStandalone)
		set := settledSet("db-server", 1)
		if mutate != nil {
			mutate(n, &set)
		}
		return observation{neo4j: n, sets: []appsv1.StatefulSet{set}}
	}
	cluster := func(readyPrimaries int32, mutate func(*neo4jv1.Neo4j)) observation {
		n := runningNeo4j(neo4jv1.TopologyModeCluster)
		set := settledSet("db-primary", 3)
		set.Status.ReadyReplicas = readyPrimaries
		if mutate != nil {
			mutate(n)
		}
		return observation{neo4j: n, sets: []appsv1.StatefulSet{set}}
	}

	tests := []struct {
		name            string
		in              observation
		converged       bool
		available       controller.Availability
		availableReason string
		messagePart     string
		operations      []string
		problemReason   string
		terminal        bool
	}{
		{
			name:      "standalone running and rolled out",
			in:        standalone(nil),
			converged: true,
			available: controller.Available,
		},
		{
			name: "standalone without a cached StatefulSet reads the operator's server summary",
			in: func() observation {
				o := standalone(nil)
				o.sets = nil
				o.neo4j.Status.ServerSummary = &neo4jv1.ReplicaSummary{Servers: 1, Ready: 1}
				return o
			}(),
			converged: true,
			available: controller.Available,
		},
		{
			name: "first install, never ready",
			in: standalone(func(n *neo4jv1.Neo4j, s *appsv1.StatefulSet) {
				n.Status.Phase = neo4jv1.Neo4jPhaseBootstrapping
				n.Status.Version = ""
				setCond(n, metav1.Condition{Type: neo4jConditionReady, Status: metav1.ConditionFalse, Reason: "MembersNotReady", Message: "0/1 servers ready", ObservedGeneration: 2})
				s.Status.ReadyReplicas = 0
			}),
			available:   controller.AvailabilityUnreported,
			messagePart: "0/1 servers ready",
		},
		{
			name: "spec change not yet observed by the operator",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				n.Generation = 3
			}),
			available:   controller.Available,
			messagePart: "apply the latest configuration",
		},
		{
			name: "pod template roll just triggered keeps serving",
			in: standalone(func(_ *neo4jv1.Neo4j, s *appsv1.StatefulSet) {
				s.Status.UpdateRevision = "r2"
				s.Status.UpdatedReplicas = 0
			}),
			available:   controller.Available,
			messagePart: "StatefulSet db-server to roll out",
		},
		{
			name: "standalone server lost after being ready",
			in: standalone(func(n *neo4jv1.Neo4j, s *appsv1.StatefulSet) {
				n.Status.Phase = neo4jv1.Neo4jPhaseDegraded
				setCond(n, metav1.Condition{Type: neo4jConditionReady, Status: metav1.ConditionFalse, Reason: "MembersNotReady", Message: "0/1 servers ready", ObservedGeneration: 2})
				s.Status.ReadyReplicas = 0
			}),
			available:       controller.Unavailable,
			availableReason: reasonNoServerReady,
			messagePart:     "0/1 servers ready",
			problemReason:   reasonServersLost,
		},
		{
			name:      "cluster with a majority of primaries",
			in:        cluster(3, nil),
			converged: true,
			available: controller.Available,
		},
		{
			name: "cluster rolling with two of three primaries ready stays available",
			in: cluster(2, func(n *neo4jv1.Neo4j) {
				setCond(n, metav1.Condition{Type: neo4jConditionReady, Status: metav1.ConditionFalse, Reason: "MembersNotReady", Message: "2/3 servers ready", ObservedGeneration: 2})
			}),
			available:   controller.Available,
			messagePart: "2/3 servers ready",
		},
		{
			name: "cluster without primary quorum",
			in: cluster(1, func(n *neo4jv1.Neo4j) {
				setCond(n, metav1.Condition{Type: neo4jConditionReady, Status: metav1.ConditionFalse, Reason: "MembersNotReady", Message: "1/3 servers ready", ObservedGeneration: 2})
			}),
			available:       controller.Unavailable,
			availableReason: reasonNoPrimaryQuorum,
			messagePart:     "1/3 servers ready",
		},
		{
			name: "cluster without a cached StatefulSet reads the operator's member report",
			in: func() observation {
				o := cluster(0, nil)
				o.sets = nil
				o.neo4j.Status.Members = []neo4jv1.MemberStatus{
					{Name: "db-primary-0", Pool: memberPoolPrimary, PodReady: true},
					{Name: "db-primary-1", Pool: memberPoolPrimary, PodReady: true},
					{Name: "db-primary-2", Pool: memberPoolPrimary},
					{Name: "db-read-0", Pool: "read", PodReady: true},
				}
				return o
			}(),
			converged: true,
			available: controller.Available,
		},
		{
			name: "rolling version upgrade",
			in: cluster(3, func(n *neo4jv1.Neo4j) {
				n.Spec.Version = "2026.08.0"
				n.Status.Upgrade = &neo4jv1.UpgradeStatus{
					Phase: neo4jv1.UpgradePhaseRolling, PreviousVersion: testVersion, TargetVersion: "2026.08.0",
					Progress: &neo4jv1.UpgradeProgress{Total: 3, Upgraded: 1},
				}
			}),
			available:   controller.Available,
			messagePart: "Upgrading Neo4j from 2026.07.1 to 2026.08.0 (1 of 3 servers upgraded)",
			operations:  []string{corev1alpha1.ConditionUpgrading},
		},
		{
			name: "storage resize",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				setCond(n, metav1.Condition{Type: neo4jConditionStorageReady, Status: metav1.ConditionFalse, Reason: neo4jReasonStorageResizing, Message: "resizing data to 20Gi"})
				setCond(n, metav1.Condition{Type: neo4jConditionReady, Status: metav1.ConditionFalse, Reason: "StorageNotReady", Message: "1/1 servers ready", ObservedGeneration: 2})
			}),
			available:   controller.Available,
			messagePart: "Waiting for Neo4j servers",
			operations:  []string{corev1alpha1.ConditionStorageResizing},
		},
		{
			name: "operator refuses a downgrade of the current spec",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				setCond(n, metav1.Condition{Type: neo4jConditionError, Status: metav1.ConditionTrue, Reason: "VersionDowngradeRefused", Message: "downgrade refused", ObservedGeneration: 2})
			}),
			converged:     true,
			available:     controller.Available,
			problemReason: "VersionDowngradeRefused",
			terminal:      true,
		},
		{
			name: "refusal about an older spec is not terminal",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				setCond(n, metav1.Condition{Type: neo4jConditionError, Status: metav1.ConditionTrue, Reason: "VersionDowngradeRefused", Message: "downgrade refused", ObservedGeneration: 1})
			}),
			converged:     true,
			available:     controller.Available,
			problemReason: "VersionDowngradeRefused",
		},
		{
			name: "write conflicts are no problem",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				setCond(n, metav1.Condition{Type: neo4jConditionError, Status: metav1.ConditionTrue, Reason: "ReconcileFailed", Message: "Operation cannot be fulfilled on neo4js.neo4j.com \"db\""})
			}),
			converged: true,
			available: controller.Available,
		},
		{
			name: "plugin download failure",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				setCond(n, metav1.Condition{Type: neo4jConditionPluginsReady, Status: metav1.ConditionFalse, Reason: "PluginDownloadFailed", Message: "apoc: 404"})
			}),
			converged:     true,
			available:     controller.Available,
			problemReason: "PluginDownloadFailed",
		},
		{
			name: "offline maintenance set outside OpenEverest",
			in: standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
				n.Spec.Maintenance = &neo4jv1.MaintenanceSpec{OfflineMode: true}
			}),
			available:       controller.Unavailable,
			availableReason: reasonOfflineMaintenance,
			messagePart:     "offline maintenance",
			problemReason:   reasonOfflineMaintenance,
			terminal:        true,
		},
		{
			name: "restore replaces the data and hides operator errors",
			in: func() observation {
				o := standalone(func(n *neo4jv1.Neo4j, _ *appsv1.StatefulSet) {
					setCond(n, metav1.Condition{Type: neo4jConditionError, Status: metav1.ConditionTrue, Reason: "ReconcileFailed", Message: "bolt unavailable"})
				})
				o.restore = &restoreInProgress{reason: corev1alpha1.ConditionRestoring, message: "Restore r1 is running"}
				return o
			}(),
			available:       controller.Unavailable,
			availableReason: corev1alpha1.ConditionRestoring,
			messagePart:     "Restore r1 is running",
			operations:      []string{corev1alpha1.ConditionRestoring},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := observe(tt.in)
			assert.Equal(t, tt.converged, st.Converged, "message: %s", st.Message)
			assert.Equal(t, tt.available, st.Available)
			if tt.availableReason != "" {
				assert.Equal(t, tt.availableReason, st.AvailableReason)
			}
			if tt.converged {
				assert.Empty(t, st.Message)
			} else {
				assert.Contains(t, st.Message, tt.messagePart)
			}
			assert.Equal(t, tt.operations, operationTypes(st))
			if tt.problemReason == "" {
				assert.Empty(t, st.Problems)
				return
			}
			if assert.Len(t, st.Problems, 1) {
				assert.Equal(t, tt.problemReason, st.Problems[0].Reason)
				assert.Equal(t, tt.terminal, st.Problems[0].Terminal)
			}
		})
	}
}

func TestWithDesiredReplicas(t *testing.T) {
	instance := &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: common.TopologyCluster},
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentEngine: {},
				common.ComponentRead:   {Replicas: ptr.To[int32](2)},
			},
		},
	}
	st := withDesiredReplicas(testContext(t, instance), controller.Status{})
	assert.ElementsMatch(t, []controller.ComponentReplicas{
		{Component: common.ComponentEngine, Desired: 3},
		{Component: common.ComponentRead, Desired: 2},
	}, st.DesiredReplicas)

	instance.Spec.Topology = &corev1alpha1.TopologySpec{Type: common.TopologyStandalone}
	st = withDesiredReplicas(testContext(t, instance), controller.Status{})
	assert.Equal(t, []controller.ComponentReplicas{{Component: common.ComponentEngine, Desired: 1}}, st.DesiredReplicas)
}
