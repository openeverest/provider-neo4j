package provider

import (
	"context"
	"errors"
	"fmt"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

// releaseAnnotation records the provider release that last applied the Neo4j
// resource. The operator does not copy it to the pods.
const releaseAnnotation = "neo4j.openeverest.io/provider-release"

// upgradeTokenPrefix prefixes the maintenance token of a provider upgrade; the
// rest is the release that is upgraded to.
const upgradeTokenPrefix = "upgrade-to-"

// holdProviderUpgrade reports whether desired may be applied. A new provider
// release can build a different Neo4j resource for an unchanged Instance, and
// the operator restarts the pods for nearly any change to it. Such a change
// waits for maintenance approval: until then the Neo4j resource keeps what
// the previous release applied, including any edit made meanwhile. Once a
// release has applied it, every later change comes from the owner and applies
// right away, as before.
func holdProviderUpgrade(c *controller.Context, desired *neo4jv1.Neo4j) (bool, error) {
	release, err := providerRelease(c)
	if err != nil {
		return false, err
	}
	if release == "" {
		return true, nil
	}
	if desired.Annotations == nil {
		desired.Annotations = map[string]string{}
	}
	desired.Annotations[releaseAnnotation] = release

	live := &neo4jv1.Neo4j{}
	if err := c.Get(live, desired.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, fmt.Errorf("get Neo4j: %w", err)
	}
	if live.Annotations[releaseAnnotation] == release {
		return true, nil
	}
	changed, err := changesSpec(c, desired, live)
	if err != nil {
		return false, err
	}
	if !changed {
		return true, nil
	}
	severity, impact := controller.MaintenanceRollingRestart, "causes a rolling restart"
	if memberCount(live.Spec.Topology) < 2 {
		severity, impact = controller.MaintenanceDowntime, "restarts the only server"
	}
	description := fmt.Sprintf("Apply the Neo4j configuration of provider release %s (%s)", release, impact)
	return c.RequestMaintenance(upgradeTokenPrefix+release, description, severity), nil
}

func providerRelease(c *controller.Context) (string, error) {
	spec, err := c.ProviderSpec()
	if err != nil {
		// The token must not change while the action is pending, so a failed
		// read fails the Sync rather than skipping the gate.
		return "", fmt.Errorf("get provider spec for the release: %w", err)
	}
	if spec.Release == nil {
		return "", nil
	}
	return spec.Release.Version, nil
}

// changesSpec reports whether applying desired would change the spec of the
// live Neo4j resource. A server-side dry run sees what the API server would
// store: operator defaults stay and fields the provider stops setting go.
func changesSpec(c *controller.Context, desired, live *neo4jv1.Neo4j) (bool, error) {
	capture := &applyCapture{Client: client.NewDryRunClient(c.Client())}
	dryRun := controller.NewContext(c.Context(), capture, c.Instance(), common.ProviderName)
	if err := dryRun.Apply(desired.DeepCopy()); err != nil {
		return false, fmt.Errorf("dry-run apply Neo4j: %w", err)
	}
	if capture.applied == nil {
		return false, errors.New("dry-run apply Neo4j returned no object")
	}
	applied := &neo4jv1.Neo4j{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(capture.applied, applied); err != nil {
		return false, fmt.Errorf("decode dry-run Neo4j: %w", err)
	}
	return !equality.Semantic.DeepEqual(applied.Spec, live.Spec), nil
}

// applyCapture keeps the object the API server returns for an apply.
type applyCapture struct {
	client.Client
	applied map[string]any
}

func (a *applyCapture) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
	if err := a.Client.Apply(ctx, obj, opts...); err != nil {
		return err
	}
	if u, ok := obj.(interface{ UnstructuredContent() map[string]any }); ok {
		a.applied = u.UnstructuredContent()
	}
	return nil
}
