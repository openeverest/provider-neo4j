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

	// TLS configures client and cluster encryption. TLS is enabled by default.
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

// TLSParameters configures transport security.
type TLSParameters struct {
	// Mode enables TLS for client Bolt/HTTPS connections. Enabled also enables
	// verified Bolt for operator administration of clusters. Disabled is
	// intended only for evaluation and cannot be used with Cluster.
	// +kubebuilder:default=enabled
	// +optional
	Mode TLSMode `json:"mode,omitempty"`

	// Issuer optionally selects an existing cert-manager Issuer. When omitted,
	// the provider creates a self-signed Issuer for the Instance.
	// +optional
	Issuer *IssuerReference `json:"issuer,omitempty"`

	// CertificateSecretName optionally names a BYO TLS Secret. It must contain
	// tls.crt, tls.key and ca.crt and is used for Bolt, HTTPS and cluster TLS.
	// +optional
	CertificateSecretName string `json:"certificateSecretName,omitempty"`
}

// TLSMode controls client and member encryption.
// +kubebuilder:validation:Enum=enabled;disabled
type TLSMode string

const (
	TLSModeEnabled  TLSMode = "enabled"
	TLSModeDisabled TLSMode = "disabled"
)

// IssuerKind selects a cert-manager issuer scope.
// +kubebuilder:validation:Enum=Issuer;ClusterIssuer
type IssuerKind string

const (
	IssuerKindIssuer        IssuerKind = "Issuer"
	IssuerKindClusterIssuer IssuerKind = "ClusterIssuer"
)

// IssuerReference identifies a cert-manager issuer.
type IssuerReference struct {
	// Name is the issuer name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// +kubebuilder:default=Issuer
	// +optional
	Kind IssuerKind `json:"kind,omitempty"`
}
