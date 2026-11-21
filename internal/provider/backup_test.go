package provider

import (
	"context"
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-neo4j/internal/common"
)

// The operator fails a restore that asks for metadata from an object-storage
// backup, after it has replaced the data.
func TestSyncRestoreIgnoresRestoreMetadata(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, neo4jv1.AddToScheme(scheme))
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default"}}
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
	}
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "rollback", Namespace: "default", UID: "restore-uid"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "graph"},
			DataSource: backupv1alpha1.DataSource{
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
			},
		},
	}
	restore.Spec.Parameters = rawExtension(t, map[string]any{"overwrite": true, "restoreMetadata": true})
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(instance, backup, restore).WithStatusSubresource(backup).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)

	status, err := (&Provider{}).SyncRestore(c, restore)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.RestoreStatePending, status.State)
	assert.Equal(t, restoreMetadataIgnored, status.Message)

	got := &neo4jv1.Neo4jRestore{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "rollback"}, got))
	assert.False(t, got.Spec.RestoreMetadata)
	assert.True(t, got.Spec.Overwrite)
	assert.Equal(t, "nightly", got.Spec.Source.BackupRef)
}

func TestBackupRetention(t *testing.T) {
	t.Parallel()
	count := int32(5)
	tests := []struct {
		name  string
		input *corev1alpha1.BackupScheduleRetention
		last  *int32
		days  *int32
	}{
		{
			name:  "count",
			input: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: &count},
			last:  &count,
		},
		{
			name:  "days",
			input: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeTime, Duration: "30d"},
			days:  int32Pointer(30),
		},
		{
			name:  "weeks",
			input: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeTime, Duration: "4w"},
			days:  int32Pointer(28),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := backupRetention(tc.input)
			require.NoError(t, err)
			assert.Equal(t, tc.last, got.KeepLast)
			assert.Equal(t, tc.days, got.KeepDays)
		})
	}
}

func TestBackupRetentionRejectsEmptyDuration(t *testing.T) {
	t.Parallel()
	_, err := backupRetention(&corev1alpha1.BackupScheduleRetention{
		Type: corev1alpha1.BackupScheduleRetentionTypeTime,
	})
	require.Error(t, err)
}

func TestDefaultBackupDatabaseIsRestorable(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []string{"neo4j"}, defaultBackupDatabases(nil))
}

func int32Pointer(value int32) *int32 { return &value }
