package provider

import (
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/internal/common"
)

func tlsInstance(t *testing.T, topology string, tls map[string]any) *corev1alpha1.Instance {
	t.Helper()
	params := map[string]any{}
	if topology == common.TopologyCluster {
		params["edition"] = "enterprise"
		params["licenseAcceptance"] = "yes"
	}
	if tls != nil {
		params["tls"] = tls
	}
	engine := validEngine()
	if topology == common.TopologyStandalone {
		one := int32(1)
		engine.Replicas = &one
	}
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"},
		Spec: corev1alpha1.InstanceSpec{
			ProviderRef: commonv1alpha1.ObjectRef{Name: common.ProviderName},
			Topology:    &corev1alpha1.TopologySpec{Type: topology},
			Version:     "2026.07.1",
			Parameters:  rawExtension(t, params),
			Components:  map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
		},
	}
}

func errorFields(errs field.ErrorList) []string {
	var out []string
	for _, err := range errs {
		out = append(out, err.Field)
	}
	return out
}

func TestValidateTLS(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		topology string
		tls      map[string]any
		want     []string
	}{
		{name: "default", topology: common.TopologyStandalone},
		{name: "require", topology: common.TopologyStandalone, tls: map[string]any{"mode": "requireTLS"}},
		{name: "disabled standalone", topology: common.TopologyStandalone, tls: map[string]any{"mode": "disabled"}},
		{
			name: "issuer and additional names", topology: common.TopologyCluster,
			tls: map[string]any{
				"issuer":          map[string]any{"name": "company-ca", "kind": "ClusterIssuer", "group": "cert-manager.io"},
				"additionalNames": []string{"graph.example.com", "*.graph.example.com"},
			},
		},
		{
			name: "additional names with the managed CA", topology: common.TopologyStandalone,
			tls: map[string]any{"additionalNames": []string{"graph.example.com"}},
		},
		{
			name: "removed enabled mode", topology: common.TopologyStandalone,
			tls:  map[string]any{"mode": "enabled"},
			want: []string{"spec.parameters.tls.mode"},
		},
		{
			name: "preferTLS is not supported", topology: common.TopologyStandalone,
			tls:  map[string]any{"mode": "preferTLS"},
			want: []string{"spec.parameters.tls.mode"},
		},
		{
			name: "disabled cluster", topology: common.TopologyCluster,
			tls:  map[string]any{"mode": "disabled"},
			want: []string{"spec.parameters.tls.mode"},
		},
		{
			name: "disabled with certificate settings", topology: common.TopologyStandalone,
			tls:  map[string]any{"mode": "disabled", "additionalNames": []string{"graph.example.com"}},
			want: []string{"spec.parameters.tls"},
		},
		{
			name: "external issuer group", topology: common.TopologyStandalone,
			tls:  map[string]any{"issuer": map[string]any{"name": "pca", "kind": "AWSPCAClusterIssuer", "group": "awspca.cert-manager.io"}},
			want: []string{"spec.parameters.tls.issuer.kind", "spec.parameters.tls.issuer.group"},
		},
		{
			name: "issuer kind without a name", topology: common.TopologyStandalone,
			tls:  map[string]any{"issuer": map[string]any{"kind": "ClusterIssuer"}},
			want: []string{"spec.parameters.tls.issuer.name"},
		},
		{
			name: "empty issuer is ignored", topology: common.TopologyStandalone,
			tls: map[string]any{"issuer": map[string]any{"name": ""}, "certificateSecretName": "graph-tls"},
		},
		{
			name: "issuer and certificate Secret", topology: common.TopologyStandalone,
			tls:  map[string]any{"issuer": map[string]any{"name": "ca"}, "certificateSecretName": "graph-tls"},
			want: []string{"spec.parameters.tls.issuer"},
		},
		{
			name: "additional names and certificate Secret", topology: common.TopologyStandalone,
			tls:  map[string]any{"certificateSecretName": "graph-tls", "additionalNames": []string{"graph.example.com"}},
			want: []string{"spec.parameters.tls.additionalNames"},
		},
		{
			name: "invalid additional names", topology: common.TopologyStandalone,
			tls: map[string]any{"additionalNames": []string{"10.0.0.1", "Not_A_Name", "a.example.com", "A.example.com", "a.example.com"}},
			want: []string{
				"spec.parameters.tls.additionalNames[0]",
				"spec.parameters.tls.additionalNames[1]",
				"spec.parameters.tls.additionalNames[3]",
				"spec.parameters.tls.additionalNames[4]",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := tlsInstance(t, tt.topology, tt.tls)
			res := (&Provider{}).Validate(controller.ValidationInput{Instance: in})
			assert.Equal(t, tt.want, errorFields(res.Errors), "errors: %v", res.Errors)
		})
	}
}

func TestValidateTLSTransition(t *testing.T) {
	t.Parallel()
	issuer := map[string]any{"name": "company-ca", "kind": "ClusterIssuer"}
	tests := []struct {
		name     string
		old, new map[string]any
		want     []string
	}{
		{name: "turn TLS on later", old: map[string]any{"mode": "disabled"}, new: map[string]any{"mode": "requireTLS", "issuer": issuer}},
		{
			name: "turn TLS off", old: nil, new: map[string]any{"mode": "disabled"},
			want: []string{"spec.parameters.tls.mode"},
		},
		{
			name: "migrate from enabled keeps the issuer",
			old:  map[string]any{"mode": "enabled", "issuer": issuer},
			new:  map[string]any{"mode": "requireTLS", "issuer": map[string]any{"name": "company-ca", "kind": "ClusterIssuer", "group": "cert-manager.io"}},
		},
		{
			name: "change the issuer", old: map[string]any{"issuer": issuer}, new: nil,
			want: []string{"spec.parameters.tls"},
		},
		{
			name: "switch to a certificate Secret", old: nil, new: map[string]any{"certificateSecretName": "graph-tls"},
			want: []string{"spec.parameters.tls"},
		},
		{name: "change additional names", old: nil, new: map[string]any{"additionalNames": []string{"graph.example.com"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			old := tlsInstance(t, common.TopologyStandalone, tt.old)
			in := tlsInstance(t, common.TopologyStandalone, tt.new)
			res := (&Provider{}).Validate(controller.ValidationInput{Instance: in, Old: old})
			assert.Equal(t, tt.want, errorFields(res.Errors), "errors: %v", res.Errors)
		})
	}
}

func TestBuildTLSWithIssuerAndAdditionalNames(t *testing.T) {
	t.Parallel()
	in := tlsInstance(t, common.TopologyStandalone, map[string]any{
		"mode":            "requireTLS",
		"issuer":          map[string]any{"name": "company-ca", "kind": "ClusterIssuer"},
		"additionalNames": []string{"graph.example.com"},
	})
	got, err := buildTLS(testContext(t, in))
	require.NoError(t, err)
	assert.True(t, got.Enabled)
	require.NotNil(t, got.CertManager)
	assert.Equal(t, &neo4jv1.IssuerRef{Name: "company-ca", Kind: "ClusterIssuer"}, got.CertManager.IssuerRef)
	assert.Equal(t, []string{"graph.example.com"}, got.CertManager.DNSNames)
}

func TestBuildTLSDisabled(t *testing.T) {
	t.Parallel()
	got, err := buildTLS(testContext(t, tlsInstance(t, common.TopologyStandalone, map[string]any{"mode": "disabled"})))
	require.NoError(t, err)
	assert.False(t, got.Enabled)
	assert.Nil(t, got.CertManager)
}

func TestApproveAdditionalNames(t *testing.T) {
	t.Parallel()
	names := []string{"graph.example.com"}
	liveNeo4j := func(enabled bool) *neo4jv1.Neo4j {
		return &neo4jv1.Neo4j{
			ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default"},
			Spec: neo4jv1.Neo4jSpec{Trust: &neo4jv1.TrustSpec{
				Enabled:     enabled,
				CertManager: &neo4jv1.CertManagerSpec{Enabled: true},
			}},
		}
	}
	run := func(t *testing.T, approved string, live *neo4jv1.Neo4j) (*controller.Context, []string) {
		t.Helper()
		in := tlsInstance(t, common.TopologyStandalone, map[string]any{"additionalNames": names})
		in.Spec.Maintenance = &corev1alpha1.MaintenanceSpec{Approved: approved}
		var c *controller.Context
		if live != nil {
			c = testContext(t, in, live)
		} else {
			c = testContext(t, in)
		}
		desired, err := buildNeo4j(c)
		require.NoError(t, err)
		require.NoError(t, approveAdditionalNames(c, desired))
		return c, desired.Spec.Trust.CertManager.DNSNames
	}

	t.Run("new instance applies the names", func(t *testing.T) {
		t.Parallel()
		c, got := run(t, "", nil)
		assert.Equal(t, names, got)
		assert.Empty(t, c.GetPendingMaintenance())
	})
	t.Run("running instance waits for approval", func(t *testing.T) {
		t.Parallel()
		c, got := run(t, "", liveNeo4j(true))
		assert.Empty(t, got)
		pending := c.GetPendingMaintenance()
		require.Len(t, pending, 1)
		assert.Equal(t, additionalNamesToken(names), pending[0].ApprovalToken)
		assert.Equal(t, controller.MaintenanceRollingRestart, pending[0].Severity)
	})
	t.Run("approved change applies the names", func(t *testing.T) {
		t.Parallel()
		c, got := run(t, additionalNamesToken(names), liveNeo4j(true))
		assert.Equal(t, names, got)
		assert.Empty(t, c.GetPendingMaintenance())
	})
	t.Run("turning TLS on applies the names", func(t *testing.T) {
		t.Parallel()
		c, got := run(t, "", liveNeo4j(false))
		assert.Equal(t, names, got)
		assert.Empty(t, c.GetPendingMaintenance())
	})
}

func TestNormalizeTLSDefaultsToRequire(t *testing.T) {
	t.Parallel()
	assert.Equal(t, definition.TLSModeRequire, normalizeTLS(definition.Parameters{}).Mode)
}
