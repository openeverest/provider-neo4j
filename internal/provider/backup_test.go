package provider

import (
	"context"
	"testing"

	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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

// A new Neo4j has an empty "neo4j" database, so a seed that does not
// overwrite it fails with DatabaseExists.
func TestSyncRestoreSeedOverwritesTheEmptyDatabase(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, neo4jv1.AddToScheme(scheme))
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default", UID: "graph-uid"}}
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: "default"},
		Status:     backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
	}
	newRestore := func(name string, owned bool) *backupv1alpha1.Restore {
		restore := &backupv1alpha1.Restore{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name)},
			Spec: backupv1alpha1.RestoreSpec{
				InstanceRef: commonv1alpha1.ObjectRef{Name: "graph"},
				DataSource: backupv1alpha1.DataSource{
					Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
				},
			},
		}
		if owned {
			require.NoError(t, controllerutil.SetControllerReference(instance, restore, scheme))
		}
		return restore
	}
	seed := newRestore("graph-datasource", true)
	// A user's Restore with the seed's name is not the seed.
	manual := newRestore("other-datasource", false)
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithObjects(instance, backup, seed, manual).WithStatusSubresource(backup).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)

	for _, tc := range []struct {
		restore   *backupv1alpha1.Restore
		overwrite bool
	}{{seed, true}, {manual, false}} {
		_, err := (&Provider{}).SyncRestore(c, tc.restore)
		require.NoError(t, err)
		got := &neo4jv1.Neo4jRestore{}
		require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tc.restore.Name}, got))
		assert.Equal(t, tc.overwrite, got.Spec.Overwrite, tc.restore.Name)
		assert.Equal(t, tc.overwrite, got.Spec.ForceOffline, tc.restore.Name)
	}
}

// A schedule prunes the Neo4jBackups it emitted. The Backup that mirrored one
// must go with it: taking the backup again would store a new full backup
// under an old name, outside the schedule's retention.
func TestSyncBackupDropsAPrunedScheduledBackup(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, backupv1alpha1.AddToScheme(scheme))
	require.NoError(t, neo4jv1.AddToScheme(scheme))
	instance := &corev1alpha1.Instance{ObjectMeta: metav1.ObjectMeta{Name: "graph", Namespace: "default"}}
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "graph-nightly-20261121-0200-f", Namespace: "default"},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeInstance, InstanceRef: &commonv1alpha1.ObjectRef{Name: "graph"},
			},
			StorageRef:   commonv1alpha1.ObjectRef{Name: "s3"},
			ScheduleName: "nightly",
		},
		Status: backupv1alpha1.BackupStatus{State: backupv1alpha1.BackupStateSucceeded},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(instance, backup).WithStatusSubresource(backup).Build()
	c := controller.NewContext(context.Background(), cl, instance, common.ProviderName)

	status, err := (&Provider{}).SyncBackup(c, backup)
	require.NoError(t, err)
	assert.Equal(t, backupv1alpha1.BackupStateSucceeded, status.State)

	key := types.NamespacedName{Namespace: "default", Name: backup.Name}
	assert.True(t, apierrors.IsNotFound(cl.Get(context.Background(), key, &backupv1alpha1.Backup{})), "the Backup is deleted")
	assert.True(t, apierrors.IsNotFound(cl.Get(context.Background(), key, &neo4jv1.Neo4jBackup{})), "and no backup is taken")
}

func TestBackupScheduleSpec(t *testing.T) {
	t.Parallel()
	destination := neo4jv1.BackupDestination{Type: neo4jv1.BackupDestinationS3, URL: "s3://bucket/graph"}
	count := int32(4)
	schedule := corev1alpha1.InstanceBackupSchedule{
		Name: "weekly", Enabled: true, Cron: "0 2 * * 0",
		Retention: &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: &count},
	}

	// Without parameters a schedule renders what it rendered before they
	// existed, so upgrading the provider changes no Neo4jBackupSchedule.
	got, err := backupScheduleSpec("graph", schedule, destination)
	require.NoError(t, err)
	assert.Equal(t, neo4jv1.Neo4jBackupScheduleSpec{
		Neo4jRef: neo4jv1.Neo4jRef{Name: "graph"},
		Full:     neo4jv1.FullCadence{Schedule: "0 2 * * 0", Retention: &neo4jv1.BackupRetention{KeepLast: &count}},
		BackupTemplate: neo4jv1.BackupTemplate{
			Databases:   []string{"neo4j"},
			Destination: destination,
		},
	}, got)

	schedule.Enabled = false
	schedule.Parameters = rawExtension(t, map[string]any{
		"databases":           []string{"orders", "customers"},
		"incrementalSchedule": "0 2 * * 1-6",
		"aggregate":           true,
		"scratchSize":         "200Gi",
		// A schedule's cron always takes full backups.
		"type": "Incremental",
	})
	got, err = backupScheduleSpec("graph", schedule, destination)
	require.NoError(t, err)
	assert.True(t, got.Suspend)
	assert.Equal(t, "0 2 * * 0", got.Full.Schedule)
	assert.Equal(t, []string{"orders", "customers"}, got.BackupTemplate.Databases)
	require.NotNil(t, got.Incremental)
	assert.Equal(t, "0 2 * * 1-6", got.Incremental.Schedule)
	require.NotNil(t, got.Aggregate)
	assert.True(t, got.Aggregate.Enabled)
	require.NotNil(t, got.BackupTemplate.Scratch)
	assert.Equal(t, "200Gi", got.BackupTemplate.Scratch.Size)
}

func TestValidateRejectsBrokenIncrementalSchedule(t *testing.T) {
	t.Parallel()
	one := int32(1)
	engine := validEngine()
	engine.Replicas = &one
	instance := &corev1alpha1.Instance{
		Spec: corev1alpha1.InstanceSpec{
			Topology:   &corev1alpha1.TopologySpec{Type: common.TopologyStandalone},
			Parameters: rawExtension(t, map[string]any{"edition": "enterprise", "licenseAcceptance": "eval"}),
			Components: map[string]corev1alpha1.ComponentSpec{common.ComponentEngine: engine},
			Backup: &corev1alpha1.InstanceBackupSpec{
				Enabled: true,
				Storages: []corev1alpha1.InstanceBackupStorage{{
					Schedules: []corev1alpha1.InstanceBackupSchedule{
						{Name: "ok", Cron: "0 2 * * 0", Parameters: rawExtension(t, map[string]any{"incrementalSchedule": "0 2 * * 1-6"})},
						{Name: "broken", Cron: "0 3 * * 0", Parameters: rawExtension(t, map[string]any{"incrementalSchedule": "hourly"})},
					},
				}},
			},
		},
	}
	result := (&Provider{}).Validate(controller.ValidationInput{Instance: instance})
	require.Len(t, result.Errors, 1)
	assert.Equal(t, "spec.backup.storages[0].schedules[1].parameters.incrementalSchedule", result.Errors[0].Field)
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
