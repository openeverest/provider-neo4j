package provider

import (
	"os"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// Dependabot bumps the operator subchart but not the go.mod replacement that
// pins its API types, so a chart bump alone must not pass CI: the CRDs in the
// chart are copied from the Go module.
func TestOperatorChartAndAPIModuleArePinnedTogether(t *testing.T) {
	t.Parallel()
	gomod, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	match := regexp.MustCompile(`(?m)^replace github\.com/neo4j/neo4j-kubernetes-operator => \S+ v(\S+)$`).
		FindSubmatch(gomod)
	require.NotNil(t, match, "go.mod must pin the Neo4j operator API module with a replace directive")
	apiVersion := string(match[1])

	data, err := os.ReadFile("../../charts/provider-neo4j/Chart.yaml")
	require.NoError(t, err)
	var chart struct {
		Dependencies []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	require.NoError(t, yaml.Unmarshal(data, &chart))
	require.Len(t, chart.Dependencies, 1)
	assert.Equal(t, "neo4j-operator", chart.Dependencies[0].Name)
	assert.Equal(t, apiVersion, chart.Dependencies[0].Version,
		"bump the replace directive in go.mod to the subchart's version and run `make generate`")
}
