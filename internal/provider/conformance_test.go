package provider

import (
	"testing"

	monitoringv1alpha1 "github.com/openeverest/openeverest/v2/api/monitoring/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

var unverifiable = map[string]string{
	// The harness sets one field at a time; the kind takes effect only
	// together with issuer.name (TestBuildTLSWithIssuerAndAdditionalNames
	// covers it).
	"spec.parameters.tls.issuer.kind": "applies only with issuer.name set",
	// The harness sets the default (requireTLS), which renders the same CR as
	// leaving it unset; TestBuildTLSDisabled covers the other value.
	"spec.parameters.tls.mode": "the probed value equals the default",
	// Gates creation in Validate only; it never reaches the engine CR.
	"spec.parameters.licenseAcceptance": "checked by Validate, not rendered",
}

func TestUISchemaIsReconciled(t *testing.T) {
	p := New()
	// The runtime registers the monitoring API itself; the harness only
	// knows the provider's own scheme.
	p.SchemeFuncs = append(p.SchemeFuncs, monitoringv1alpha1.AddToScheme)
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider:     p,
		Unverifiable: unverifiable,
	})
}
