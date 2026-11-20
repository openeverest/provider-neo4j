package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

// Labels the Neo4j operator puts on the StatefulSets of each server pool.
const (
	labelInstance = "app.kubernetes.io/instance"
	labelPool     = "neo4j.com/pool"
)

// Conditions and reasons the Neo4j operator writes on the Neo4j object.
const (
	neo4jConditionReady         = "Ready"
	neo4jConditionError         = "Error"
	neo4jConditionStorageReady  = "StorageReady"
	neo4jConditionTLSReady      = "TLSReady"
	neo4jConditionPluginsReady  = "PluginsReady"
	neo4jConditionClusterFormed = "ClusterFormed"

	neo4jReasonStorageResizing = "StorageResizing"
)

// Reasons of the facts the provider reports itself. A problem's reason
// becomes the status word users read, so each names one cause.
const (
	reasonOfflineMaintenance = "OfflineMaintenance"
	reasonNoServerReady      = "NoServerReady"
	reasonNoPrimaryQuorum    = "NoPrimaryQuorum"
	reasonClusterNotFormed   = "ClusterNotFormed"
	reasonRollingUpgrade     = "RollingUpgrade"
	reasonUpgradeFailed      = "UpgradeFailed"
	reasonServersLost        = "ServersLost"
)

// errFragmentConflict is how the API server refuses a write made from a
// stale read. The operator fails its pass on one and retries at once, so it
// is no failure.
const errFragmentConflict = "Operation cannot be fulfilled"

// terminalErrorReasons are the operator's Error reasons that only a person can
// resolve: the operator refuses what the spec asks for and will keep refusing.
var terminalErrorReasons = []string{
	"VersionDowngradeRefused",
	"VersionUpgradeRefused",
	"StorageTemplateDrift",
}

// problemConditionReasons are the reasons the operator gives a False
// TLSReady, StorageReady, PluginsReady or ClusterFormed condition that
// describe a failure rather than a step still under way.
var problemConditionReasons = map[string][]string{
	neo4jConditionTLSReady:      {"SecretMissing"},
	neo4jConditionStorageReady:  {"StorageResizeFailed"},
	neo4jConditionPluginsReady:  {"PluginDownloadFailed", "PluginVersionIncompatible", "PluginJarUnreadable"},
	neo4jConditionClusterFormed: {"BootstrapGateTooHigh", "ShowServersFailed", "UnsupportedSystemScaleUp", "UnsupportedSinglePrimary"},
}

// restoreInProgress describes a restore that owns the database for now.
type restoreInProgress struct {
	reason, message string
	// seeding is true for the restore that fills a new Instance from its
	// data source.
	seeding bool
}

// observation is what the provider reads from the cluster to report status.
type observation struct {
	neo4j   *neo4jv1.Neo4j
	sets    []appsv1.StatefulSet
	restore *restoreInProgress
}

// status reports what the provider observes about Neo4j: whether it runs
// what the Instance asks for, whether clients can connect, what is wrong and
// which long operation is running, and how many pods each component will
// have. The runtime derives the Instance's phase and conditions from these
// facts.
func status(c *controller.Context) (controller.Status, error) {
	neo4j, err := uncachedNeo4j(c)
	if apierrors.IsNotFound(err) {
		return withDesiredReplicas(c, controller.Progressing("Waiting for the Neo4j object to be created")), nil
	}
	if err != nil {
		return controller.Status{}, err
	}
	restore, err := runningRestore(c)
	if err != nil {
		return controller.Status{}, err
	}
	sets := &appsv1.StatefulSetList{}
	if err := c.List(sets, client.MatchingLabels{labelInstance: neo4j.Name}, client.HasLabels{labelPool}); err != nil {
		return controller.Status{}, fmt.Errorf("list StatefulSets of Neo4j %q: %w", neo4j.Name, err)
	}

	st := withDesiredReplicas(c, observe(observation{neo4j: neo4j, sets: sets.Items, restore: restore}))

	// Clients told the database is available must find where to connect,
	// also while a change is still rolling out.
	if !st.Converged && st.Available != controller.Available {
		return st, nil
	}
	details, err := connectionDetails(c, neo4j)
	if err != nil {
		return controller.Status{}, fmt.Errorf("build connection details: %w", err)
	}
	st.ConnectionDetails = details
	return st, nil
}

// uncachedNeo4j reads the Neo4j object from the API server instead of the
// cache. Sync has just applied it, and the cache may still hold the object
// from before that, whose status would read as caught up with a generation
// that is no longer the latest. The client reads unstructured objects without
// the cache.
func uncachedNeo4j(c *controller.Context) (*neo4jv1.Neo4j, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(neo4jv1.GroupVersion.WithKind("Neo4j"))
	if err := c.Get(obj, c.Name()); err != nil {
		return nil, fmt.Errorf("get Neo4j %q: %w", c.Name(), err)
	}
	raw, err := obj.MarshalJSON()
	if err != nil {
		return nil, fmt.Errorf("encode Neo4j %q: %w", c.Name(), err)
	}
	neo4j := &neo4jv1.Neo4j{}
	if err := json.Unmarshal(raw, neo4j); err != nil {
		return nil, fmt.Errorf("decode Neo4j %q: %w", c.Name(), err)
	}
	return neo4j, nil
}

// runningRestore returns the restore in progress for the Instance, or nil.
// A restore stops and restarts Neo4j, so the Restore reaching a terminal
// state is what ends it, not what Neo4j reports meanwhile.
func runningRestore(c *controller.Context) (*restoreInProgress, error) {
	if ds := c.GetDataSourceStatus(); ds != nil && !ds.Done {
		message := ds.Message
		if message == "" {
			message = "The instance is being seeded from its data source"
		}
		return &restoreInProgress{reason: corev1alpha1.ReasonSeeding, message: message, seeding: true}, nil
	}
	name, err := activeRestoreName(c)
	if err != nil || name == "" {
		return nil, err
	}
	return &restoreInProgress{
		reason:  corev1alpha1.ConditionRestoring,
		message: fmt.Sprintf("Restore %s is running", name),
	}, nil
}

// activeRestoreName returns the name of a Restore targeting the Instance that
// has not reached a terminal state, or "" when there is none.
func activeRestoreName(c *controller.Context) (string, error) {
	restores := &backupv1alpha1.RestoreList{}
	if err := c.List(restores, client.MatchingFields{controller.IndexRestoreInstanceName: c.Name()}); err != nil {
		return "", fmt.Errorf("list Restore resources for %q: %w", c.Name(), err)
	}
	var names []string
	for i := range restores.Items {
		r := &restores.Items[i]
		if !r.DeletionTimestamp.IsZero() {
			continue
		}
		switch r.Status.State {
		case backupv1alpha1.RestoreStateSucceeded, backupv1alpha1.RestoreStateFailed:
		default:
			names = append(names, r.Name)
		}
	}
	if len(names) == 0 {
		return "", nil
	}
	return slices.Min(names), nil
}

// observe turns the Neo4j object, its StatefulSets and the restore in
// progress, if any, into the facts the provider reports.
func observe(o observation) controller.Status {
	var st controller.Status
	st.Available, st.AvailableReason, st.AvailableMessage = availability(o)

	if offlineMode(o.neo4j) {
		// The provider never sets spec.maintenance, so it was set on the Neo4j
		// object by hand and only whoever set it can bring Neo4j back.
		st = st.WithTerminalProblem(reasonOfflineMaintenance,
			"Neo4j is stopped: spec.maintenance.offlineMode is set on its Neo4j object, outside OpenEverest. Unset it to start Neo4j again.")
	}
	// A restore stops Neo4j and the operator reports the gaps; those come with
	// the restore and are not problems of their own.
	if o.restore == nil {
		for _, p := range problems(o.neo4j) {
			if p.Terminal {
				st = st.WithTerminalProblem(p.Reason, p.Message)
			} else {
				st = st.WithProblem(p.Reason, p.Message)
			}
		}
	}

	if o.restore != nil {
		st = st.WithOperation(corev1alpha1.ConditionRestoring, o.restore.reason, o.restore.message)
	}
	if up := o.neo4j.Status.Upgrade; upgradeInFlight(up) {
		st = st.WithOperation(corev1alpha1.ConditionUpgrading, reasonRollingUpgrade, upgradeMessage(o.neo4j))
	}
	if cond := condition(o.neo4j, neo4jConditionStorageReady); cond != nil &&
		cond.Status == metav1.ConditionFalse && cond.Reason == neo4jReasonStorageResizing {
		st = st.WithOperation(corev1alpha1.ConditionStorageResizing, neo4jReasonStorageResizing,
			orDefault(cond.Message, "Neo4j data volumes are being resized"))
	}

	st.Message = waitingFor(o)
	st.Converged = st.Message == ""
	return st
}

// problems lists what the operator reports as wrong with Neo4j.
func problems(neo4j *neo4jv1.Neo4j) []controller.Problem {
	var out []controller.Problem
	if cond := condition(neo4j, neo4jConditionError); cond != nil && cond.Status == metav1.ConditionTrue &&
		!strings.Contains(cond.Message, errFragmentConflict) {
		// Terminal only when the refusal is about the current spec: right
		// after a fix the error still showing may be the one being fixed.
		terminal := slices.Contains(terminalErrorReasons, cond.Reason) && cond.ObservedGeneration == neo4j.Generation
		out = append(out, controller.Problem{
			Reason:   orDefault(cond.Reason, corev1alpha1.ReasonEngineError),
			Message:  orDefault(cond.Message, "The Neo4j operator reports an error without a message"),
			Terminal: terminal,
		})
	}
	for _, condType := range []string{neo4jConditionTLSReady, neo4jConditionStorageReady, neo4jConditionPluginsReady, neo4jConditionClusterFormed} {
		cond := condition(neo4j, condType)
		if cond == nil || cond.Status != metav1.ConditionFalse || !slices.Contains(problemConditionReasons[condType], cond.Reason) {
			continue
		}
		out = append(out, controller.Problem{Reason: cond.Reason, Message: orDefault(cond.Message, condType+" is "+cond.Reason)})
	}
	if up := neo4j.Status.Upgrade; up != nil && up.Phase == neo4jv1.UpgradePhaseFailed {
		out = append(out, controller.Problem{
			Reason:  reasonUpgradeFailed,
			Message: orDefault(up.LastError, "The rolling upgrade to "+up.TargetVersion+" failed"),
		})
	}
	if neo4j.Status.Phase == neo4jv1.Neo4jPhaseDegraded {
		// The operator reports Degraded only for an unplanned loss of servers
		// after Neo4j has been ready, never for a change it was asked to make.
		message := "Neo4j lost servers it had"
		if ready := condition(neo4j, neo4jConditionReady); ready != nil && ready.Message != "" {
			message += ": " + ready.Message
		}
		out = append(out, controller.Problem{Reason: reasonServersLost, Message: message})
	}
	return out
}

// waitingFor says what keeps Neo4j from running what the Instance asks for.
// It returns "" when nothing does.
func waitingFor(o observation) string {
	neo4j := o.neo4j
	switch {
	case o.restore != nil:
		return o.restore.message
	case offlineMode(neo4j):
		return "Neo4j is in offline maintenance"
	case neo4j.Status.ObservedGeneration < neo4j.Generation:
		return "Waiting for the operator to apply the latest configuration"
	}
	if up := neo4j.Status.Upgrade; upgradeInFlight(up) {
		return upgradeMessage(neo4j)
	}
	ready := condition(neo4j, neo4jConditionReady)
	switch {
	case ready == nil:
		return "Waiting for the operator to report on Neo4j"
	case ready.ObservedGeneration != 0 && ready.ObservedGeneration < neo4j.Generation:
		return "Waiting for the operator to report on the latest configuration"
	case ready.Status != metav1.ConditionTrue:
		return "Waiting for Neo4j servers to become ready (" + orDefault(ready.Message, ready.Reason) + ")"
	case neo4j.Status.Version != neo4j.Spec.Version:
		return "Waiting for Neo4j to run version " + neo4j.Spec.Version
	}
	if wait := rolloutWait(o.sets); wait != "" {
		return "Waiting for " + wait
	}
	if neo4j.Status.Phase != neo4jv1.Neo4jPhaseRunning {
		return fmt.Sprintf("Waiting for the operator to report Neo4j running (it reports %s)", neo4j.Status.Phase)
	}
	return ""
}

// rolloutWait returns what the first StatefulSet that has not finished
// rolling out is waiting for, or "" when all have. The operator decides Ready
// from ready pods, which a rolling restart that has just been triggered does
// not change yet.
func rolloutWait(sets []appsv1.StatefulSet) string {
	sets = slices.Clone(sets)
	slices.SortFunc(sets, func(a, b appsv1.StatefulSet) int { return strings.Compare(a.Name, b.Name) })
	for i := range sets {
		set := &sets[i]
		if !set.DeletionTimestamp.IsZero() {
			continue
		}
		want := int32(1)
		if set.Spec.Replicas != nil {
			want = *set.Spec.Replicas
		}
		s := set.Status
		switch {
		case s.ObservedGeneration < set.Generation:
			return fmt.Sprintf("StatefulSet %s to pick up its new pod template", set.Name)
		case s.Replicas != want:
			return fmt.Sprintf("StatefulSet %s to scale to %d pods (it has %d)", set.Name, want, s.Replicas)
		case s.UpdateRevision != "" && s.CurrentRevision != s.UpdateRevision:
			return fmt.Sprintf("StatefulSet %s to roll out (%d of %d pods updated)", set.Name, s.UpdatedReplicas, s.Replicas)
		case s.ReadyReplicas < s.Replicas:
			return fmt.Sprintf("StatefulSet %s to become ready (%d of %d pods ready)", set.Name, s.ReadyReplicas, s.Replicas)
		}
	}
	return ""
}

// availability says whether clients can connect. A standalone server needs
// its pod ready; a cluster needs a formed cluster and a majority of its
// primaries ready to take writes. Before Neo4j has first been ready, the
// answer is left to the runtime, which reports it unavailable.
func availability(o observation) (controller.Availability, string, string) {
	neo4j := o.neo4j
	switch {
	case offlineMode(neo4j):
		return controller.Unavailable, reasonOfflineMaintenance, "Neo4j is in offline maintenance"
	case o.restore != nil && o.restore.seeding:
		return controller.Unavailable, corev1alpha1.ReasonLoadingData, "The instance is being seeded from its data source"
	case o.restore != nil:
		return controller.Unavailable, corev1alpha1.ConditionRestoring, "A restore is replacing the data"
	}
	established := neo4j.Status.Version != ""

	if neo4j.Spec.Topology.Mode != neo4jv1.TopologyModeCluster {
		// The operator counts ready servers from the same StatefulSet; its
		// summary stands in while the StatefulSet is not in the cache yet.
		var ready int32
		if set := findSet(o.sets, neo4j.Name+"-server"); set != nil {
			ready = set.Status.ReadyReplicas
		} else if summary := neo4j.Status.ServerSummary; summary != nil {
			ready = summary.Ready
		}
		switch {
		case ready > 0:
			return controller.Available, "", ""
		case !established:
			return controller.AvailabilityUnreported, "", ""
		default:
			return controller.Unavailable, reasonNoServerReady, "The Neo4j server is not ready"
		}
	}

	if formed := condition(neo4j, neo4jConditionClusterFormed); formed == nil || formed.Status != metav1.ConditionTrue {
		if !established {
			return controller.AvailabilityUnreported, "", ""
		}
		message := "The Neo4j cluster is not formed"
		if formed != nil && formed.Message != "" {
			message += ": " + formed.Message
		}
		return controller.Unavailable, reasonClusterNotFormed, message
	}
	desired := int32(1)
	if neo4j.Spec.Topology.Primaries != nil && neo4j.Spec.Topology.Primaries.Members > 0 {
		desired = neo4j.Spec.Topology.Primaries.Members
	}
	quorum := desired/2 + 1
	var ready int32
	if set := findSet(o.sets, neo4j.Name+"-primary"); set != nil {
		ready = set.Status.ReadyReplicas
	}
	if ready >= quorum {
		return controller.Available, "", ""
	}
	if !established {
		return controller.AvailabilityUnreported, "", ""
	}
	return controller.Unavailable, reasonNoPrimaryQuorum,
		fmt.Sprintf("%d of %d primaries are ready; writes need %d", ready, desired, quorum)
}

// withDesiredReplicas reports how many pods each component will have once
// Neo4j runs what the Instance asks for, with the defaults Sync applies.
func withDesiredReplicas(c *controller.Context, st controller.Status) controller.Status {
	if c.Instance().GetTopologyType() != common.TopologyCluster {
		return st.WithDesiredReplicas(common.ComponentEngine, 1)
	}
	components := c.Instance().Spec.Components
	st = st.WithDesiredReplicas(common.ComponentEngine, replicaCount(components[common.ComponentEngine], 3))
	for _, name := range []string{common.ComponentAnalytics, common.ComponentRead} {
		if component, ok := components[name]; ok {
			st = st.WithDesiredReplicas(name, replicaCount(component, 1))
		}
	}
	return st
}

func upgradeInFlight(up *neo4jv1.UpgradeStatus) bool {
	return up != nil && up.Phase != "" && up.Phase != neo4jv1.UpgradePhaseCompleted && up.Phase != neo4jv1.UpgradePhaseFailed
}

func upgradeMessage(neo4j *neo4jv1.Neo4j) string {
	up := neo4j.Status.Upgrade
	message := "Upgrading Neo4j"
	if up.PreviousVersion != "" {
		message += " from " + up.PreviousVersion
	}
	if target := orDefault(up.TargetVersion, neo4j.Spec.Version); target != "" {
		message += " to " + target
	}
	if p := up.Progress; p != nil && p.Total > 0 {
		message += fmt.Sprintf(" (%d of %d servers upgraded)", p.Upgraded, p.Total)
	}
	return message
}

func offlineMode(neo4j *neo4jv1.Neo4j) bool {
	return neo4j.Spec.Maintenance != nil && neo4j.Spec.Maintenance.OfflineMode
}

func condition(neo4j *neo4jv1.Neo4j, condType string) *metav1.Condition {
	return meta.FindStatusCondition(neo4j.Status.Conditions, condType)
}

func findSet(sets []appsv1.StatefulSet, name string) *appsv1.StatefulSet {
	for i := range sets {
		if sets[i].Name == name && sets[i].DeletionTimestamp.IsZero() {
			return &sets[i]
		}
	}
	return nil
}

func orDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

// enqueueStatefulSetInstance maps a StatefulSet the Neo4j operator runs for a
// pool to its Instance. Whether the Instance has converged depends on the
// StatefulSets having rolled out, and the Neo4j status may not change again
// once they have.
func enqueueStatefulSetInstance() func(context.Context, client.Object) []reconcile.Request {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		name := labels[labelInstance]
		if name == "" || labels[labelPool] == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: name}}}
	}
}
