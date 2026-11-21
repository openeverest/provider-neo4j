package provider

import (
	"context"
	"fmt"
	"hash/fnv"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

var _ controller.BackupImporter = (*Provider)(nil)

// artifactName matches the files neo4j-admin writes, for example
// "neo4j-2026-11-21T09-20-06.backup": the database and when the backup started.
var artifactName = regexp.MustCompile(`^(.+)-(\d{4}-\d{2}-\d{2}T\d{2}-\d{2}-\d{2})\.backup$`)

const artifactTimeLayout = "2006-01-02T15-04-05"

// SetClient gives the provider the client it lists BackupStorages with. Call
// it before the reconciler starts.
func (p *Provider) SetClient(c client.Client) { p.client = c }

// ImportBackups lists the Neo4j backup artifacts in the BackupStorage and
// returns an external Backup for each. An incremental artifact is restorable
// too: Neo4j replays its chain from the same directory.
func (p *Provider) ImportBackups(
	ctx context.Context,
	imp *backupv1alpha1.BackupImport,
	storage *backupv1alpha1.BackupStorage,
) (controller.BackupImportExecutionStatus, error) {
	s3c, err := controller.NewS3Client(ctx, p.client, storage)
	if err != nil {
		return controller.BackupImportExecutionStatus{}, fmt.Errorf("build S3 client: %w", err)
	}
	bucket := storage.Spec.Location.ObjectStore.Bucket
	// Backup paths are relative to the storage's location.
	prefix := ""
	if storage.Spec.Location.ObjectStore.Prefix != "" {
		prefix = strings.Trim(storage.Spec.Location.ObjectStore.Prefix, "/") + "/"
	}

	var backups []*backupv1alpha1.Backup
	paginator := s3.NewListObjectsV2Paginator(s3c, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
		Prefix: aws.String(prefix),
	})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return controller.BackupImportExecutionStatus{}, fmt.Errorf("list objects in bucket %q: %w", bucket, err)
		}
		for _, obj := range page.Contents {
			key := strings.TrimPrefix(aws.ToString(obj.Key), prefix)
			if backup := importedBackup(imp, key, aws.ToTime(obj.LastModified)); backup != nil {
				backups = append(backups, backup)
			}
		}
	}
	log.FromContext(ctx).Info("discovered Neo4j backup artifacts",
		"backupImport", imp.Name, "storage", storage.Name, "count", len(backups))
	return controller.BackupImportExecutionStatus{
		Backups: backups,
		State:   backupv1alpha1.BackupImportStateSucceeded,
	}, nil
}

// importedBackup builds the external Backup for the artifact at key, or
// returns nil when key is not a Neo4j backup artifact. The name is derived
// from the storage and the path, so importing again creates no duplicates.
func importedBackup(imp *backupv1alpha1.BackupImport, key string, modified time.Time) *backupv1alpha1.Backup {
	match := artifactName.FindStringSubmatch(path.Base(key))
	if match == nil {
		return nil
	}
	started, err := time.Parse(artifactTimeLayout, match[2])
	if err != nil {
		return nil
	}
	completed := modified
	if completed.Before(started) {
		completed = started
	}
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      importedBackupName(imp.Spec.StorageRef.Name, key),
			Namespace: imp.Namespace,
		},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeExternal,
				External: &backupv1alpha1.BackupOriginExternal{
					Path:        key,
					StartedAt:   metav1.NewTime(started),
					CompletedAt: metav1.NewTime(completed),
				},
			},
			ClassRef:       imp.Spec.ClassRef,
			StorageRef:     imp.Spec.StorageRef,
			DeletionPolicy: backupv1alpha1.BackupDeletionPolicyRetain,
		},
	}
}

func importedBackupName(storageName, key string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(storageName))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(key))
	return fmt.Sprintf("import-%016x", h.Sum64())
}

// externalRestoreSource points a Neo4jRestore at the artifact of an external
// Backup. The servers read it with the credentials of the Instance's backup
// storage, so the storage must be registered on the Instance.
func externalRestoreSource(c *controller.Context, backup *backupv1alpha1.Backup) (neo4jv1.RestoreSource, string, error) {
	external := backup.Spec.Origin.External
	if external == nil {
		return neo4jv1.RestoreSource{}, "", fmt.Errorf("external Backup %q has no origin.external", backup.Name)
	}
	storage, err := resolveStorage(c, backup.Spec.StorageRef.Name)
	if err != nil {
		return neo4jv1.RestoreSource{}, "", err
	}
	if !storage.Apply() {
		return neo4jv1.RestoreSource{}, "", fmt.Errorf("BackupStorage %q is not ready: %s", storage.Name(), storage.Message)
	}
	location := storage.Storage.Spec.Location.ObjectStore
	url, database, err := artifactURL(location.Bucket, location.Prefix, external.Path)
	if err != nil {
		return neo4jv1.RestoreSource{}, "", fmt.Errorf("external Backup %q: %w", backup.Name, err)
	}
	return neo4jv1.RestoreSource{Type: neo4jv1.BackupDestinationS3, URL: url}, database, nil
}

// artifactURL returns the S3 URL of the artifact at key, which is relative to
// the storage's prefix, and the database the artifact holds.
func artifactURL(bucket, prefix, key string) (url, database string, err error) {
	match := artifactName.FindStringSubmatch(path.Base(key))
	if match == nil {
		return "", "", fmt.Errorf("%q is not a Neo4j backup artifact (<database>-<time>.backup)", key)
	}
	return "s3://" + path.Join(bucket, prefix, strings.Trim(key, "/")), match[1], nil
}
