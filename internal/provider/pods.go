package provider

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

// componentPools maps each component of the Instance to the Neo4j server pool
// that runs it.
func componentPools(c *controller.Context) map[string]string {
	if c.Instance().GetTopologyType() != common.TopologyCluster {
		return map[string]string{common.ComponentEngine: "server"}
	}
	pools := map[string]string{common.ComponentEngine: "primary"}
	for _, name := range []string{common.ComponentAnalytics, common.ComponentRead} {
		if _, ok := c.Instance().Spec.Components[name]; ok {
			pools[name] = name
		}
	}
	return pools
}

// labelPods puts the OpenEverest pod labels on the pods of each pool. The
// runtime counts a component's pods, and accepts metrics endpoints, by these
// labels, and the Neo4j operator offers no way to render them into its pod
// templates. Pods it recreates lose them until the next reconcile, which the
// StatefulSet watch triggers.
func labelPods(c *controller.Context) error {
	for component, pool := range componentPools(c) {
		want := c.PodLabels(component)
		pods := &corev1.PodList{}
		if err := c.List(pods, client.MatchingLabels{labelInstance: c.Name(), labelPool: pool}); err != nil {
			return fmt.Errorf("list pods of pool %s: %w", pool, err)
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			if hasLabels(pod.Labels, want) || !pod.DeletionTimestamp.IsZero() {
				continue
			}
			patch := client.MergeFrom(pod.DeepCopy())
			if pod.Labels == nil {
				pod.Labels = map[string]string{}
			}
			maps.Copy(pod.Labels, want)
			if err := c.Client().Patch(c.Context(), pod, patch); client.IgnoreNotFound(err) != nil {
				return fmt.Errorf("label pod %s: %w", pod.Name, err)
			}
		}
	}
	return nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}
