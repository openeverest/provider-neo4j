package provider

import (
	"fmt"
	"slices"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/definition"
	"github.com/openeverest/provider-neo4j/internal/common"
)

const internalIssuerSuffix = "-selfsigned"

func tlsParameters(c *controller.Context) (definition.TLSParameters, error) {
	params, err := instanceParameters(c)
	if err != nil {
		return definition.TLSParameters{}, err
	}
	tls := definition.TLSParameters{}
	if params.TLS != nil {
		tls = *params.TLS
	}
	if tls.Mode == "" {
		tls.Mode = definition.TLSModeEnabled
	}
	if tls.Issuer != nil && tls.Issuer.Kind == "" {
		tls.Issuer.Kind = definition.IssuerKindIssuer
	}
	return tls, nil
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
	if tls.Mode == definition.TLSModeDisabled || tls.CertificateSecretName != "" || tls.Issuer != nil {
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
	if tls.Mode == definition.TLSModeDisabled {
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

func validateTLS(in *corev1alpha1.Instance, params definition.Parameters) field.ErrorList {
	tls := definition.TLSParameters{}
	if params.TLS != nil {
		tls = *params.TLS
	}
	if tls.Mode == "" {
		tls.Mode = definition.TLSModeEnabled
	}
	path := field.NewPath("spec", "parameters", "tls")
	var errs field.ErrorList
	if !slices.Contains([]definition.TLSMode{definition.TLSModeEnabled, definition.TLSModeDisabled}, tls.Mode) {
		errs = append(errs, field.NotSupported(path.Child("mode"), tls.Mode, []definition.TLSMode{
			definition.TLSModeEnabled, definition.TLSModeDisabled,
		}))
	}
	if tls.Issuer != nil && tls.CertificateSecretName != "" {
		errs = append(errs, field.Forbidden(path.Child("issuer"), "use either an issuer or a certificate Secret, not both"))
	}
	if tls.Mode == definition.TLSModeDisabled && in.GetTopologyType() == common.TopologyCluster {
		errs = append(errs, field.Forbidden(path.Child("mode"), "Cluster requires TLS so the operator can use verified admin Bolt"))
	}
	return errs
}

func validateTLSReferences(c *controller.Context) (field.ErrorList, error) {
	tls, err := tlsParameters(c)
	if err != nil {
		return nil, err
	}
	path := field.NewPath("spec", "parameters", "tls")
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
