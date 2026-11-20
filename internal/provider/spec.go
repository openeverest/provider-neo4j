package provider

import (
	"fmt"
	"strings"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	corev1 "k8s.io/api/core/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/definition/components"
	"github.com/openeverest/provider-neo4j/internal/common"
)

func instanceParameters(c *controller.Context) (definition.Parameters, error) {
	params := definition.Parameters{}
	if !c.TryDecodeParameters(&params) {
		return params, nil
	}
	return params, nil
}

func effectiveEdition(params definition.Parameters) definition.Edition {
	if params.Edition == "" {
		return definition.EditionCommunity
	}
	return params.Edition
}

func buildNeo4j(c *controller.Context) (*neo4jv1.Neo4j, error) {
	in := c.Instance()
	params, err := instanceParameters(c)
	if err != nil {
		return nil, err
	}
	edition := effectiveEdition(params)
	engine := in.Spec.Components[common.ComponentEngine]
	engineVersion, image, err := neo4jVersion(engine)
	if err != nil {
		return nil, err
	}
	if engineVersion == "" {
		providerSpec, err := c.ProviderSpec()
		if err != nil {
			return nil, err
		}
		defaultImage := controller.GetDefaultImageForComponent(providerSpec, common.ComponentEngine)
		_, engineVersion, err = splitImage(defaultImage)
		if err != nil {
			return nil, err
		}
	}

	mode := neo4jv1.TopologyModeStandalone
	topology := neo4jv1.TopologySpec{Mode: mode}
	if in.GetTopologyType() == common.TopologyCluster {
		mode = neo4jv1.TopologyModeCluster
		topology.Mode = mode
		topology.Primaries = &neo4jv1.PrimariesSpec{
			Members: replicaCount(engine, 3),
		}
		applyPoolParameters(c, &topology.Primaries.Plugins, &topology.Primaries.Resources, engine)
		topology.Secondaries = buildSecondaryPools(c)
		topology.MinimumMembers = minimumMembers(topology.Primaries.Members)
		topology.DefaultPrimariesCount = minimumMembers(topology.Primaries.Members)
	}

	resources := corev1.ResourceRequirements{}
	if engine.Resources != nil {
		resources = *engine.Resources.DeepCopy()
	}
	storage, err := buildStorage(engine.Storage)
	if err != nil {
		return nil, err
	}
	auth := &neo4jv1.AuthSpec{GeneratePassword: boolPtr(true)}
	if in.Spec.UserSecretRef != nil {
		auth = &neo4jv1.AuthSpec{PasswordSecretRef: &neo4jv1.PasswordSecretRef{
			Name: authSecretName(c),
		}}
	}
	connectivity := buildConnectivity(engine)
	tls, err := buildTLS(c)
	if err != nil {
		return nil, err
	}
	metrics, err := metricsRequired(c)
	if err != nil {
		return nil, err
	}
	features := &neo4jv1.FeaturesSpec{}
	if in.Spec.Backup != nil && in.Spec.Backup.Enabled && edition == definition.EditionEnterprise {
		features.Backup = &neo4jv1.BackupFeatureSpec{Enabled: true}
		connectivity.Listeners.Backup = int32Ptr(6362)
	}
	if metrics {
		features.Monitoring = &neo4jv1.MonitoringFeaturesSpec{
			Prometheus: &neo4jv1.PrometheusMonitoringSpec{Enabled: true},
		}
		connectivity.Listeners.Metrics = int32Ptr(2004)
	}
	if features.Backup == nil && features.Monitoring == nil {
		// An empty struct reaches the API server as null, which the CRD rejects.
		features = nil
	}

	spec := neo4jv1.Neo4jSpec{
		Edition:      neo4jv1.Edition(edition),
		Version:      engineVersion,
		Topology:     topology,
		Image:        image,
		Auth:         auth,
		Storage:      storage,
		Resources:    resources,
		Features:     features,
		Trust:        tls,
		Connectivity: connectivity,
		Scheduling:   buildScheduling(engine),
		Security:     &neo4jv1.SecuritySpec{},
	}
	if edition == definition.EditionEnterprise {
		spec.License = &neo4jv1.LicenseSpec{Accept: neo4jv1.LicenseAccept(params.LicenseAcceptance)}
	}
	applyStandaloneParameters(c, &spec, engine)
	if err := configureBackupStorage(c, &spec); err != nil {
		return nil, err
	}

	return &neo4jv1.Neo4j{
		ObjectMeta: c.ObjectMeta(c.Name()),
		Spec:       spec,
	}, nil
}

func applyStandaloneParameters(c *controller.Context, spec *neo4jv1.Neo4jSpec, component corev1alpha1.ComponentSpec) {
	if spec.Topology.Mode != neo4jv1.TopologyModeStandalone {
		applyGlobalConfig(c, spec, component)
		return
	}
	params := components.Neo4jParameters{}
	_ = c.TryDecodeComponentParameters(component, &params)
	spec.Plugins = params.Plugins
	applyGlobalConfig(c, spec, component)
}

func applyGlobalConfig(c *controller.Context, spec *neo4jv1.Neo4jSpec, component corev1alpha1.ComponentSpec) {
	params := components.Neo4jParameters{}
	_ = c.TryDecodeComponentParameters(component, &params)
	spec.Config = &neo4jv1.ConfigSpec{
		Neo4j: params.Configuration,
		JVM:   &neo4jv1.JVMSpec{AdditionalArguments: params.JVMAdditionalArguments},
	}
}

func applyPoolParameters(c *controller.Context, plugins *[]string, resources **corev1.ResourceRequirements, component corev1alpha1.ComponentSpec) {
	params := components.Neo4jParameters{}
	_ = c.TryDecodeComponentParameters(component, &params)
	*plugins = params.Plugins
	if component.Resources != nil {
		*resources = component.Resources.DeepCopy()
	}
}

func buildSecondaryPools(c *controller.Context) *neo4jv1.SecondariesSpec {
	out := &neo4jv1.SecondariesSpec{}
	if component, ok := c.Instance().Spec.Components[common.ComponentAnalytics]; ok {
		out.Analytics = &neo4jv1.SecondaryPoolSpec{Members: replicaCount(component, 1)}
		applyPoolParameters(c, &out.Analytics.Plugins, &out.Analytics.Resources, component)
	}
	if component, ok := c.Instance().Spec.Components[common.ComponentRead]; ok {
		out.Read = &neo4jv1.SecondaryPoolSpec{Members: replicaCount(component, 1)}
		applyPoolParameters(c, &out.Read.Plugins, &out.Read.Resources, component)
	}
	if out.Analytics == nil && out.Read == nil {
		return nil
	}
	return out
}

func buildStorage(storage *corev1alpha1.Storage) (*neo4jv1.StorageSpec, error) {
	size := "10Gi"
	class := ""
	if storage != nil {
		if !storage.Size.IsZero() {
			size = storage.Size.String()
		}
		if storage.StorageClass != nil {
			class = *storage.StorageClass
		}
	}
	return &neo4jv1.StorageSpec{
		Volumes: &neo4jv1.VolumesSpec{
			Data: neo4jv1.DataVolumeSpec{
				Mode: neo4jv1.VolumeModeDynamic,
				Dynamic: &neo4jv1.DynamicVolumeSpec{
					Size:             size,
					StorageClassName: class,
					AccessMode:       "ReadWriteOnce",
				},
			},
		},
	}, nil
}

func buildConnectivity(engine corev1alpha1.ComponentSpec) *neo4jv1.ConnectivitySpec {
	service := &neo4jv1.ConnectivityServiceSpec{
		Type:   neo4jv1.ServiceType(corev1.ServiceTypeClusterIP),
		Expose: []string{"bolt", "http", "https"},
	}
	if engine.Service != nil {
		service.Type = neo4jv1.ServiceType(engine.Service.ServiceType)
		service.Annotations = engine.Service.Annotations
		if engine.Service.LoadBalancerService != nil {
			service.LoadBalancerSourceRanges = engine.Service.LoadBalancerService.SourceRanges
		}
	}
	return &neo4jv1.ConnectivitySpec{
		Listeners: &neo4jv1.ConnectivityListenersSpec{
			Bolt: int32Ptr(7687), HTTP: int32Ptr(7474), HTTPS: int32Ptr(7473),
		},
		Service: service,
	}
}

func buildScheduling(engine corev1alpha1.ComponentSpec) *neo4jv1.SchedulingSpec {
	p := engine.SchedulingPolicy
	if p == nil {
		return nil
	}
	out := &neo4jv1.SchedulingSpec{
		NodeSelector: p.NodeSelector,
		Tolerations:  p.Tolerations,
		Affinity:     &neo4jv1.SchedulingAffinitySpec{Custom: p.Affinity},
	}
	if p.TopologySpreadConstraints != nil {
		out.TopologySpreadConstraints = *p.TopologySpreadConstraints
	}
	return out
}

func neo4jVersion(component corev1alpha1.ComponentSpec) (string, *neo4jv1.ImageSpec, error) {
	if component.Image == "" {
		return component.Version, nil, nil
	}
	repository, tag, err := splitImage(component.Image)
	if err != nil {
		return "", nil, err
	}
	tag = strings.TrimSuffix(tag, "-enterprise")
	return tag, &neo4jv1.ImageSpec{Repository: repository, PullPolicy: corev1.PullIfNotPresent}, nil
}

func splitImage(image string) (repository, tag string, err error) {
	if strings.Contains(image, "@") {
		return "", "", fmt.Errorf("digest image %q is not supported; provide a tagged image", image)
	}
	i := strings.LastIndex(image, ":")
	if i <= strings.LastIndex(image, "/") {
		return "", "", fmt.Errorf("image %q must include a tag", image)
	}
	return image[:i], image[i+1:], nil
}

func replicaCount(component corev1alpha1.ComponentSpec, fallback int32) int32 {
	if component.Replicas != nil {
		return *component.Replicas
	}
	return fallback
}

func minimumMembers(primaries int32) *int32 {
	n := int32(1)
	if primaries > 1 {
		n = 3
	}
	return &n
}

func metricsRequired(c *controller.Context) (bool, error) {
	required, err := c.RequiredSources()
	if err != nil {
		return false, err
	}
	metrics := required.Metrics.Else(false)
	if !metrics {
		return false, nil
	}
	params, err := instanceParameters(c)
	if err != nil {
		return false, err
	}
	if effectiveEdition(params) != definition.EditionEnterprise {
		return false, &controller.BackupConfigError{
			Reason:  "EnterpriseMonitoringRequired",
			Message: "Prometheus metrics require edition enterprise and explicit licenseAcceptance",
		}
	}
	return true, nil
}

func int32Ptr(v int32) *int32 { return &v }
func boolPtr(v bool) *bool    { return &v }
