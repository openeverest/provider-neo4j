package provider

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
)

func TestImportedBackup(t *testing.T) {
	t.Parallel()
	imp := &backupv1alpha1.BackupImport{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "team-a"},
		Spec: backupv1alpha1.BackupImportSpec{
			ClassRef:   commonv1alpha1.ObjectRef{Name: "neo4j-backup"},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
		},
	}
	modified := time.Date(2026, 11, 21, 9, 20, 12, 0, time.UTC)
	key := "prod/nightly/my-db-2026-11-21T09-20-06.backup"

	got := importedBackup(imp, key, modified)
	require.NotNil(t, got)
	assert.Equal(t, "team-a", got.Namespace)
	assert.Equal(t, backupv1alpha1.BackupOriginTypeExternal, got.Spec.Origin.Type)
	assert.Equal(t, key, got.Spec.Origin.External.Path)
	assert.Equal(t, time.Date(2026, 11, 21, 9, 20, 6, 0, time.UTC), got.Spec.Origin.External.StartedAt.UTC())
	assert.Equal(t, modified, got.Spec.Origin.External.CompletedAt.UTC())
	assert.Equal(t, "neo4j-backup", got.Spec.ClassRef.Name)
	assert.Equal(t, "s3", got.Spec.StorageRef.Name)
	// Importing never gives the provider the right to delete what it found.
	assert.Equal(t, backupv1alpha1.BackupDeletionPolicyRetain, got.Spec.DeletionPolicy)

	assert.Equal(t, got.Name, importedBackup(imp, key, modified.Add(time.Hour)).Name, "the name depends on the path only")
	assert.NotEqual(t, got.Name, importedBackup(imp, "prod/weekly/my-db-2026-11-21T09-20-06.backup", modified).Name)

	for _, other := range []string{"prod/nightly/", "prod/readme.txt", "prod/neo4j.backup", "prod/neo4j-2026-13-45T99-99-99.backup"} {
		assert.Nil(t, importedBackup(imp, other, modified), other)
	}
}

func TestArtifactURL(t *testing.T) {
	t.Parallel()
	url, database, err := artifactURL("bucket", "team/graphs", "/prod/my-db-2026-11-21T09-20-06.backup")
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/team/graphs/prod/my-db-2026-11-21T09-20-06.backup", url)
	assert.Equal(t, "my-db", database)

	url, _, err = artifactURL("bucket", "", "neo4j-2026-11-21T09-20-06.backup")
	require.NoError(t, err)
	assert.Equal(t, "s3://bucket/neo4j-2026-11-21T09-20-06.backup", url)

	_, _, err = artifactURL("bucket", "", "prod/")
	require.Error(t, err)
}
