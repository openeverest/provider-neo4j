// Package definition contains shared types used across the provider definition.
//
// +k8s:openapi-gen=true
package definition

// Parameters configures instance-wide Neo4j behavior.
type Parameters struct {
	// Edition selects Community (default) or Enterprise. Enterprise unlocks
	// clustering, backups and Prometheus metrics and requires LicenseAcceptance.
	// +kubebuilder:default=community
	// +optional
	Edition Edition `json:"edition,omitempty"`

	// LicenseAcceptance explicitly acknowledges the Neo4j Enterprise terms.
	// Set to "yes" for production or "eval" for evaluation. It is required for
	// Enterprise and forbidden for Community.
	// +optional
	LicenseAcceptance LicenseAcceptance `json:"licenseAcceptance,omitempty"`

	// TLS configures client and cluster encryption. TLS is required by
	// default.
	// +optional
	TLS *TLSParameters `json:"tls,omitempty"`
}

// Edition is a Neo4j image edition.
// +kubebuilder:validation:Enum=community;enterprise
type Edition string

const (
	EditionCommunity  Edition = "community"
	EditionEnterprise Edition = "enterprise"
)

// LicenseAcceptance records an explicit Enterprise license acknowledgement.
// +kubebuilder:validation:Enum=yes;eval
type LicenseAcceptance string

const (
	LicenseAcceptanceYes  LicenseAcceptance = "yes"
	LicenseAcceptanceEval LicenseAcceptance = "eval"
)

// TLSParameters configures TLS for the instance. The certificates come from,
// in order of precedence: certificateSecretName, the cert-manager issuer, or
// else a CA the provider creates for the instance through cert-manager. The
// certificate source is chosen when TLS is turned on and cannot change while
// it is on.
type TLSParameters struct {
	// Mode is requireTLS or disabled. requireTLS encrypts Bolt, HTTPS and,
	// on the cluster topology, the traffic between members, and rejects
	// clients without TLS. disabled is for evaluation only: the cluster
	// topology needs TLS, and TLS can be turned on later but never off.
	// preferTLS is not offered: the Neo4j operator always requires TLS on
	// Bolt once Bolt has a certificate.
	// +kubebuilder:default=requireTLS
	// +optional
	Mode TLSMode `json:"mode,omitempty"`

	// CertificateSecretName names a Secret in the instance namespace with
	// the server certificate (tls.crt), its private key (tls.key) and the CA
	// certificate it is verified with (ca.crt), labelled
	// neo4j.com/mountable=true. It is used for Bolt, HTTPS and cluster
	// traffic, so it must be valid for the instance's Service and member
	// names. Does not need cert-manager. Cannot be combined with issuer or
	// additionalNames.
	// +optional
	CertificateSecretName string `json:"certificateSecretName,omitempty"`

	// Issuer is the cert-manager issuer that signs the certificates, instead
	// of the CA the provider creates for the instance. It must write its CA
	// certificate to ca.crt, as the CA and Vault issuers do.
	// +optional
	Issuer *IssuerReference `json:"issuer,omitempty"`

	// AdditionalNames are DNS names the Bolt and HTTPS certificates are
	// valid for besides the in-cluster names, such as an external name
	// clients connect to. Cannot be combined with certificateSecretName,
	// whose certificate carries its own names. Changing them on a running
	// instance reissues the certificates and restarts the pods, so it waits
	// for maintenance approval.
	// +optional
	AdditionalNames []string `json:"additionalNames,omitempty"`
}

// TLSMode controls which connections must use TLS.
// +kubebuilder:validation:Enum=requireTLS;disabled
type TLSMode string

const (
	// TLSModeRequire encrypts all traffic and rejects clients without TLS.
	// The default.
	TLSModeRequire TLSMode = "requireTLS"
	// TLSModeDisabled turns TLS off. It can only be chosen when the instance
	// is created, and not for the cluster topology.
	TLSModeDisabled TLSMode = "disabled"
)

// IssuerKind is the kind of a cert-manager issuer.
// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
type IssuerKind string

const (
	// IssuerKindIssuer is an Issuer in the instance namespace.
	IssuerKindIssuer IssuerKind = "Issuer"
	// IssuerKindClusterIssuer is a cluster-wide ClusterIssuer.
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerReference refers to a cert-manager issuer.
type IssuerReference struct {
	// Name of the issuer. Without a name the issuer is ignored.
	// +optional
	Name string `json:"name,omitempty"`

	// Kind of the issuer.
	// +kubebuilder:default=Issuer
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`

	// Group is the API group of the issuer. Only cert-manager.io is
	// supported: the Neo4j operator does not pass a group to cert-manager,
	// so external issuers such as AWS PCA can't be used.
	// +optional
	Group string `json:"group,omitempty"`
}
