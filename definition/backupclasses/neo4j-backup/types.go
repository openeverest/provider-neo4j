// Package backupclasses contains backup and restore parameter schemas.
//
// +k8s:openapi-gen=true
package backupclasses

// Neo4jBackupParameters configures one on-demand or scheduled backup.
type Neo4jBackupParameters struct {
	// Databases defaults to the standard "neo4j" user database. The Neo4j
	// operator's restore-by-reference does not support wildcard backup
	// artifacts in v1.0.0.
	// +optional
	Databases []string `json:"databases,omitempty"`

	// Type defaults to Full. Auto is useful when Neo4j should choose a full or
	// incremental link according to the existing chain.
	// +kubebuilder:default=Full
	// +optional
	// +kubebuilder:validation:Enum=Full;Auto;Incremental
	Type string `json:"type,omitempty"`
}

// Neo4jRestoreParameters configures a restore.
// +kubebuilder:validation:XValidation:rule="!has(self.forceOffline) || !self.forceOffline || self.overwrite",message="forceOffline requires overwrite"
type Neo4jRestoreParameters struct {
	// Overwrite replaces databases that already exist.
	// +optional
	Overwrite bool `json:"overwrite,omitempty"`

	// ForceOffline stops a database before replacing it. Requires Overwrite.
	// +optional
	ForceOffline bool `json:"forceOffline,omitempty"`

	// RestoreMetadata reapplies backed-up users, roles and privileges when the
	// source supports it.
	// +optional
	RestoreMetadata bool `json:"restoreMetadata,omitempty"`

	// Databases defaults to the standard "neo4j" user database.
	// +optional
	Databases []string `json:"databases,omitempty"`
}
