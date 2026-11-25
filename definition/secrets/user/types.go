// Package user contains the schema for Neo4j bootstrap credentials.
//
// +k8s:openapi-gen=true
package user

// UserSecretData is Neo4j's conventional username/password value.
type UserSecretData struct {
	// NEO4JAuth contains "neo4j/<password>". A changed password is set in
	// Neo4j once it runs.
	NEO4JAuth string `json:"NEO4J_AUTH,omitempty"`
}
