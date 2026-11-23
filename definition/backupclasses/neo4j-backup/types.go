// Package backupclasses contains backup and restore parameter schemas.
//
// +k8s:openapi-gen=true
package backupclasses

// Neo4jBackupParameters configures one on-demand backup or one backup schedule.
type Neo4jBackupParameters struct {
	// Databases defaults to the standard "neo4j" user database. The Neo4j
	// operator's restore-by-reference does not support wildcard backup
	// artifacts in v1.0.0.
	// +optional
	Databases []string `json:"databases,omitempty"`

	// Type defaults to Full. Auto is useful when Neo4j should choose a full or
	// incremental link according to the existing chain. Ignored on a schedule,
	// whose cron always takes full backups: see IncrementalSchedule.
	// +kubebuilder:default=Full
	// +optional
	// +kubebuilder:validation:Enum=Full;Auto;Incremental
	Type string `json:"type,omitempty"`

	// IncrementalSchedule is a cron expression for incremental backups taken
	// between a schedule's full backups. Each attaches to the chain the last
	// full backup started, and retention removes whole chains. Ignored on an
	// on-demand backup.
	// +optional
	// +kubebuilder:validation:MaxLength=100
	IncrementalSchedule string `json:"incrementalSchedule,omitempty"`

	// Aggregate compacts a schedule's chain into one full backup once the next
	// full backup closes it, which bounds how long a restore replays
	// incrementals. Ignored on an on-demand backup.
	// +optional
	Aggregate bool `json:"aggregate,omitempty"`

	// ScratchSize gives the backup a dedicated staging volume of this size
	// (for example 200Gi), which must hold a compressed copy of the backup.
	// Without it the backup stages on the node's ephemeral storage and a large
	// database can get its backup pod evicted.
	// +optional
	// +kubebuilder:validation:Pattern=`^[1-9][0-9]*(Mi|Gi|Ti)$`
	ScratchSize string `json:"scratchSize,omitempty"`
}

// Neo4jRestoreParameters configures a restore.
// +kubebuilder:validation:XValidation:rule="!has(self.forceOffline) || !self.forceOffline || !has(self.overwrite) || self.overwrite",message="forceOffline requires overwrite"
type Neo4jRestoreParameters struct {
	// Overwrite replaces databases that already exist. Defaults to true: a
	// restore replaces the data of the Instance it targets, and every Neo4j
	// has the "neo4j" database. Set false to restore only databases the
	// Instance does not have yet.
	// +optional
	Overwrite *bool `json:"overwrite,omitempty"`

	// ForceOffline stops a database before replacing it. Requires Overwrite.
	// +optional
	ForceOffline bool `json:"forceOffline,omitempty"`

	// RestoreMetadata is deprecated and ignored: the Neo4j operator reapplies
	// users, roles and privileges only from PVC backups, and failed the
	// restore of an object-storage backup that asked for it.
	// +optional
	RestoreMetadata bool `json:"restoreMetadata,omitempty"`

	// Databases defaults to the standard "neo4j" user database.
	// +optional
	Databases []string `json:"databases,omitempty"`
}
