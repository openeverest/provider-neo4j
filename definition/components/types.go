// Package components contains parameter types for provider component types.
//
// +k8s:openapi-gen=true
package components

// Neo4jParameters configures the Neo4j server process for one member pool.
type Neo4jParameters struct {
	// Plugins are Neo4j image plugin catalog identifiers. In Cluster mode,
	// GDS and Bloom may only be installed on secondary pools.
	// +optional
	// +kubebuilder:validation:MaxItems=8
	// +kubebuilder:validation:items:Enum=apoc;gds;bloom
	Plugins []string `json:"plugins,omitempty"`

	// Configuration contains neo4j.conf entries.
	// +optional
	Configuration map[string]string `json:"configuration,omitempty"`

	// JVMAdditionalArguments appends JVM flags after the operator's defaults.
	// +optional
	// +kubebuilder:validation:MaxItems=64
	JVMAdditionalArguments []string `json:"jvmAdditionalArguments,omitempty"`
}
