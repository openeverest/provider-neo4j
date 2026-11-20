package provider

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	"github.com/openeverest/provider-neo4j/internal/common"
)

const (
	chartValuesPath  = "../../charts/provider-neo4j/values.yaml"
	providerSpecPath = "../../charts/provider-neo4j/generated/provider-spec.yaml"
)

type chartPreset struct {
	enabled  bool
	instance *corev1alpha1.Instance
}

// chartPresets returns the ClusterInstancePresets in the chart's values, keyed
// by name, as Instances built from their spec.
func chartPresets(t *testing.T) map[string]chartPreset {
	t.Helper()
	data, err := os.ReadFile(chartValuesPath)
	require.NoError(t, err)
	var values struct {
		Provider struct {
			Name      string `json:"name"`
			ShortName string `json:"shortName"`
		} `json:"provider"`
		Presets []struct {
			Name    string          `json:"name"`
			Enabled bool            `json:"enabled"`
			Spec    json.RawMessage `json:"spec"`
		} `json:"presets"`
	}
	require.NoError(t, yaml.Unmarshal(data, &values))
	require.Equal(t, common.ProviderName, values.Provider.Name)
	require.NotEmpty(t, values.Provider.ShortName)

	presets := map[string]chartPreset{}
	for _, p := range values.Presets {
		require.NotEmpty(t, p.Name)
		var spec corev1alpha1.ClusterInstancePresetSpec
		// Strict, like the API server: a field the Instance does not have
		// would be pruned from the preset without notice.
		require.NoError(t, yaml.UnmarshalStrict(p.Spec, &spec), "preset %s", p.Name)
		assert.Empty(t, spec.ProviderRef.Name, "preset %s: the chart sets providerRef", p.Name)
		spec.ProviderRef.Name = values.Provider.Name

		name := values.Provider.ShortName + "-" + p.Name
		presets[name] = chartPreset{enabled: p.Enabled, instance: &corev1alpha1.Instance{
			ObjectMeta: metav1.ObjectMeta{Name: "from-" + name, Namespace: "default"},
			Spec:       spec.InstanceSpec,
		}}
	}
	return presets
}

// Every topology has a preset on the provider's default version. Presets
// installed by default create a valid Instance as they are; an Enterprise
// preset ships disabled and lacks only the license acceptance, which nobody
// but the user may give.
func TestChartPresets(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(providerSpecPath)
	require.NoError(t, err)
	var provider corev1alpha1.Provider
	require.NoError(t, yaml.Unmarshal(data, &provider))
	require.NotEmpty(t, provider.Spec.DefaultVersion)

	v := validationtest.New(t, New(), providerSpecPath)
	presets := chartPresets(t)
	require.NotEmpty(t, presets)

	topologies := map[string]bool{}
	for name, p := range presets {
		in := p.instance
		t.Run(name, func(t *testing.T) {
			var params struct {
				Edition           string `json:"edition"`
				LicenseAcceptance string `json:"licenseAcceptance"`
			}
			require.NoError(t, decodeRaw(in.Spec.Parameters, &params))
			assert.Empty(t, params.LicenseAcceptance, "preset %s must not accept the Neo4j license", name)

			var got []string
			for _, err := range v.Run(in.DeepCopy(), nil).Errors {
				got = append(got, err.Field)
			}
			if params.Edition == "enterprise" {
				assert.False(t, p.enabled, "preset %s needs a license acceptance, so it ships disabled", name)
				assert.Equal(t, []string{"spec.parameters.licenseAcceptance"}, got)
				return
			}
			assert.Empty(t, got, "preset %s", name)
		})
		assert.Equal(t, provider.Spec.DefaultVersion, in.Spec.Version, "preset %s pins the default version", name)
		require.NotNil(t, in.Spec.Topology, "preset %s names its topology", name)
		topologies[in.Spec.Topology.Type] = true
	}
	for topology := range provider.Spec.Topologies {
		assert.True(t, topologies[topology], "no preset for the %s topology", topology)
	}
}
