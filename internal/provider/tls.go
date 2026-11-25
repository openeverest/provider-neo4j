package provider

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"slices"
	"strings"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/internal/common"
)

const (
	internalIssuerSuffix = "-selfsigned"
	certManagerGroup     = "cert-manager.io"
	// tlsNamesTokenPrefix prefixes the maintenance token of an
	// additionalNames change; the rest is a hash of the names.
	tlsNamesTokenPrefix = "tls-names-"
)

var tlsPath = field.NewPath("spec", "parameters", "tls")

func tlsParameters(c *controller.Context) (definition.TLSParameters, error) {
	params, err := instanceParameters(c)
	if err != nil {
		return definition.TLSParameters{}, err
	}
	return normalizeTLS(params), nil
}

// normalizeTLS applies the defaults: requireTLS, and an issuer of kind
// Issuer. An issuer without a name, as the UI may leave behind, is ignored.
func normalizeTLS(params definition.Parameters) definition.TLSParameters {
	tls := definition.TLSParameters{}
	if params.TLS != nil {
		tls = *params.TLS
	}
	if tls.Mode == "" {
		tls.Mode = definition.TLSModeRequire
	}
	if tls.Issuer != nil && tls.Issuer.Name == "" {
		tls.Issuer = nil
	}
	if tls.Issuer != nil {
		issuer := *tls.Issuer
		issuer.Kind = cmp.Or(issuer.Kind, definition.IssuerKindIssuer)
		issuer.Group = cmp.Or(issuer.Group, certManagerGroup)
		tls.Issuer = &issuer
	}
	return tls
}

func tlsEnabled(tls definition.TLSParameters) bool {
	return tls.Mode != definition.TLSModeDisabled
}

func issuerRef(c *controller.Context, tls definition.TLSParameters) *neo4jv1.IssuerRef {
	if tls.Issuer != nil {
		return &neo4jv1.IssuerRef{
			Name: tls.Issuer.Name,
			Kind: string(tls.Issuer.Kind),
		}
	}
	return &neo4jv1.IssuerRef{
		Name: c.Name() + internalIssuerSuffix,
		Kind: string(definition.IssuerKindIssuer),
	}
}

func ensureTLSIssuer(c *controller.Context) error {
	tls, err := tlsParameters(c)
	if err != nil {
		return err
	}
	if !tlsEnabled(tls) || tls.CertificateSecretName != "" || tls.Issuer != nil {
		return nil
	}
	bootstrap := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(c.Name() + internalIssuerSuffix + "-bootstrap"),
		Spec: cmapi.IssuerSpec{
			IssuerConfig: cmapi.IssuerConfig{
				SelfSigned: &cmapi.SelfSignedIssuer{},
			},
		},
	}
	if err := c.Apply(bootstrap); err != nil {
		return err
	}
	caName := c.Name() + internalIssuerSuffix + "-ca"
	certificate := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(caName),
		Spec: cmapi.CertificateSpec{
			IsCA:       true,
			CommonName: c.Name() + " Neo4j CA",
			SecretName: caName,
			Usages: []cmapi.KeyUsage{
				cmapi.UsageCertSign, cmapi.UsageCRLSign,
			},
			IssuerRef: cmmeta.IssuerReference{
				Name:  bootstrap.Name,
				Kind:  "Issuer",
				Group: "cert-manager.io",
			},
		},
	}
	if err := c.Apply(certificate); err != nil {
		return err
	}
	issuer := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(c.Name() + internalIssuerSuffix),
		Spec: cmapi.IssuerSpec{
			IssuerConfig: cmapi.IssuerConfig{
				CA: &cmapi.CAIssuer{SecretName: caName},
			},
		},
	}
	return c.Apply(issuer)
}

func buildTLS(c *controller.Context) (*neo4jv1.TrustSpec, error) {
	tls, err := tlsParameters(c)
	if err != nil {
		return nil, err
	}
	if !tlsEnabled(tls) {
		out := &neo4jv1.TrustSpec{Enabled: false}
		if c.Instance().GetTopologyType() == common.TopologyCluster {
			out.InsecureAdminConnection = true
		}
		return out, nil
	}

	trust := &neo4jv1.TrustSpec{
		Enabled: true,
		Certificates: &neo4jv1.TrustCertificatesSpec{
			Bolt:  tlsPolicy(c, tls, "bolt"),
			HTTPS: tlsPolicy(c, tls, "https"),
		},
	}
	if tls.CertificateSecretName == "" {
		trust.CertManager = &neo4jv1.CertManagerSpec{
			Enabled:   true,
			IssuerRef: issuerRef(c, tls),
			DNSNames:  slices.Clone(tls.AdditionalNames),
		}
	}
	if c.Instance().GetTopologyType() == common.TopologyCluster {
		trust.Certificates.Cluster = tlsPolicy(c, tls, "cluster")
		trust.Certificates.Cluster.ClientAuth = neo4jv1.TLSClientAuth("Require")
	}
	return trust, nil
}

func tlsPolicy(c *controller.Context, tls definition.TLSParameters, policy string) *neo4jv1.TLSPolicySpec {
	name := tlsSecretName(c, tls, policy)
	if tls.CertificateSecretName == "" {
		out := &neo4jv1.TLSPolicySpec{SecretName: name, ClientAuth: "None"}
		if policy == "cluster" {
			out.TrustedCerts = trustedCerts(name)
		}
		return out
	}
	return &neo4jv1.TLSPolicySpec{
		PrivateKey: &neo4jv1.TLSSecretKeyRef{
			SecretName: name,
			SubPath:    "tls.key",
		},
		PublicCertificate: &neo4jv1.TLSSecretKeyRef{
			SecretName: name,
			SubPath:    "tls.crt",
		},
		ClientAuth:   "None",
		TrustedCerts: trustedCerts(name),
	}
}

func trustedCerts(name string) *neo4jv1.TLSTrustedCertsSpec {
	return &neo4jv1.TLSTrustedCertsSpec{
		Sources: []corev1.VolumeProjection{{
			Secret: &corev1.SecretProjection{
				LocalObjectReference: corev1.LocalObjectReference{Name: name},
				Items: []corev1.KeyToPath{{
					Key: "ca.crt", Path: "ca.crt",
				}},
			},
		}},
	}
}

func tlsSecretName(c *controller.Context, tls definition.TLSParameters, policy string) string {
	if tls.CertificateSecretName != "" {
		return tls.CertificateSecretName
	}
	return c.Name() + "-" + policy + "-tls"
}

// approveAdditionalNames keeps the additional names of the running Neo4j's
// certificates until a change is approved: cert-manager reissues the Bolt and
// HTTPS certificates for the new names and the operator restarts the pods to
// serve them. A Neo4j that is created, or gets TLS now, starts or restarts
// anyway, so its names apply without approval.
func approveAdditionalNames(c *controller.Context, desired *neo4jv1.Neo4j) error {
	if desired.Spec.Trust == nil || desired.Spec.Trust.CertManager == nil {
		return nil
	}
	live := &neo4jv1.Neo4j{}
	if err := c.Get(live, desired.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get Neo4j: %w", err)
	}
	if live.Spec.Trust == nil || !live.Spec.Trust.Enabled || live.Spec.Trust.CertManager == nil {
		return nil
	}
	current := live.Spec.Trust.CertManager.DNSNames
	wanted := desired.Spec.Trust.CertManager.DNSNames
	if slices.Equal(current, wanted) {
		return nil
	}
	description := "Reissue the TLS certificates without additional names (rolling restart)"
	if len(wanted) > 0 {
		description = fmt.Sprintf("Reissue the TLS certificates for the additional names %s (rolling restart)",
			strings.Join(wanted, ", "))
	}
	if !c.RequestMaintenance(additionalNamesToken(wanted), description, controller.MaintenanceRollingRestart) {
		desired.Spec.Trust.CertManager.DNSNames = slices.Clone(current)
	}
	return nil
}

// additionalNamesToken identifies a change to names by the names it leads to.
func additionalNamesToken(names []string) string {
	sum := sha256.Sum256([]byte(strings.Join(names, ",")))
	return tlsNamesTokenPrefix + hex.EncodeToString(sum[:])[:10]
}

func validateTLS(in *corev1alpha1.Instance, params definition.Parameters) field.ErrorList {
	var errs field.ErrorList
	if params.TLS != nil && params.TLS.Issuer != nil && params.TLS.Issuer.Name == "" &&
		(params.TLS.Issuer.Kind != "" || params.TLS.Issuer.Group != "") {
		errs = append(errs, field.Required(tlsPath.Child("issuer", "name"), "name the issuer"))
	}
	tls := normalizeTLS(params)
	switch tls.Mode {
	case definition.TLSModeRequire, definition.TLSModeDisabled:
	default:
		errs = append(errs, field.NotSupported(tlsPath.Child("mode"), tls.Mode, []definition.TLSMode{
			definition.TLSModeRequire, definition.TLSModeDisabled,
		}))
	}
	if tls.Issuer != nil {
		if !slices.Contains([]definition.IssuerKind{definition.IssuerKindIssuer, definition.IssuerKindClusterIssuer}, tls.Issuer.Kind) {
			errs = append(errs, field.NotSupported(tlsPath.Child("issuer", "kind"), tls.Issuer.Kind, []definition.IssuerKind{
				definition.IssuerKindIssuer, definition.IssuerKindClusterIssuer,
			}))
		}
		if tls.Issuer.Group != certManagerGroup {
			errs = append(errs, field.Invalid(tlsPath.Child("issuer", "group"), tls.Issuer.Group,
				"only cert-manager.io issuers are supported: the Neo4j operator's issuerRef has no group"))
		}
	}
	if tls.Issuer != nil && tls.CertificateSecretName != "" {
		errs = append(errs, field.Forbidden(tlsPath.Child("issuer"), "use either an issuer or a certificate Secret, not both"))
	}
	if len(tls.AdditionalNames) > 0 && tls.CertificateSecretName != "" {
		errs = append(errs, field.Forbidden(tlsPath.Child("additionalNames"),
			"the certificate Secret carries its own names; add them to its certificate"))
	}
	if !tlsEnabled(tls) {
		if in.GetTopologyType() == common.TopologyCluster {
			errs = append(errs, field.Forbidden(tlsPath.Child("mode"), "Cluster requires TLS so the operator can use verified admin Bolt"))
		}
		if tls.Issuer != nil || tls.CertificateSecretName != "" || len(tls.AdditionalNames) > 0 {
			errs = append(errs, field.Forbidden(tlsPath, "certificate settings need TLS; remove them or set mode to requireTLS"))
		}
	}
	errs = append(errs, validateAdditionalNames(tls.AdditionalNames)...)
	return errs
}

// validateAdditionalNames accepts DNS names, including wildcards such as
// *.example.com, and rejects IP addresses: the operator only adds DNS names
// to the certificates.
func validateAdditionalNames(names []string) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]bool{}
	for i, name := range names {
		path := tlsPath.Child("additionalNames").Index(i)
		if net.ParseIP(name) != nil {
			errs = append(errs, field.Invalid(path, name, "must be a DNS name, not an IP address"))
			continue
		}
		if msgs := validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*.")); len(msgs) > 0 {
			for _, msg := range msgs {
				errs = append(errs, field.Invalid(path, name, msg))
			}
			continue
		}
		key := name
		if seen[key] {
			errs = append(errs, field.Duplicate(path, name))
		}
		seen[key] = true
	}
	return errs
}

// validateTLSTransition allows turning TLS on later but never off, and keeps
// the certificate source while TLS is on.
func validateTLSTransition(oldParams, params definition.Parameters) field.ErrorList {
	oldTLS, newTLS := normalizeTLS(oldParams), normalizeTLS(params)
	if !tlsEnabled(oldTLS) {
		return nil
	}
	if !tlsEnabled(newTLS) {
		return field.ErrorList{field.Forbidden(tlsPath.Child("mode"), "TLS cannot be turned off on an existing instance")}
	}
	if certificateSource(oldTLS) != certificateSource(newTLS) {
		return field.ErrorList{field.Forbidden(tlsPath, "the certificate source (issuer or certificateSecretName) cannot change while TLS is on")}
	}
	return nil
}

func certificateSource(tls definition.TLSParameters) string {
	switch {
	case tls.CertificateSecretName != "":
		return "secret/" + tls.CertificateSecretName
	case tls.Issuer != nil:
		return "issuer/" + tls.Issuer.Group + "/" + string(tls.Issuer.Kind) + "/" + tls.Issuer.Name
	}
	return "managed"
}

func validateTLSReferences(c *controller.Context) (field.ErrorList, error) {
	tls, err := tlsParameters(c)
	if err != nil {
		return nil, err
	}
	path := tlsPath
	if tls.CertificateSecretName != "" {
		secret := &corev1.Secret{}
		if err := c.Get(secret, tls.CertificateSecretName); err != nil {
			if apierrors.IsNotFound(err) {
				return field.ErrorList{field.NotFound(path.Child("certificateSecretName"), tls.CertificateSecretName)}, nil
			}
			return nil, fmt.Errorf("get TLS Secret %q: %w", tls.CertificateSecretName, err)
		}
		if secret.Labels[mountableSecretLabel] != "true" {
			return field.ErrorList{field.Invalid(path.Child("certificateSecretName"), tls.CertificateSecretName,
				fmt.Sprintf("Secret must have label %s=true", mountableSecretLabel))}, nil
		}
		for _, key := range []string{"tls.key", "tls.crt", "ca.crt"} {
			if len(secret.Data[key]) == 0 {
				return field.ErrorList{field.Invalid(path.Child("certificateSecretName"), tls.CertificateSecretName,
					"missing data key "+key)}, nil
			}
		}
	}
	if tls.Issuer != nil {
		keyPath := path.Child("issuer", "name")
		var obj client.Object = &cmapi.Issuer{}
		if tls.Issuer.Kind == definition.IssuerKindClusterIssuer {
			obj = &cmapi.ClusterIssuer{}
		}
		key := client.ObjectKey{Name: tls.Issuer.Name}
		if tls.Issuer.Kind != definition.IssuerKindClusterIssuer {
			key.Namespace = c.Namespace()
		}
		if err := c.Client().Get(c.Context(), key, obj); err != nil {
			if apierrors.IsNotFound(err) {
				return field.ErrorList{field.NotFound(keyPath, tls.Issuer.Name)}, nil
			}
			return nil, fmt.Errorf("get cert-manager issuer %q: %w", tls.Issuer.Name, err)
		}
	}
	return nil, nil
}
