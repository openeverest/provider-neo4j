package provider

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

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

func int32Pointer(value int32) *int32 { return &value }
