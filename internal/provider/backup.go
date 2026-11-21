package provider

import (
	"context"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	neo4jv1 "github.com/neo4j/neo4j-kubernetes-operator/src/api/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	neobackup "github.com/openeverest/provider-neo4j/definition/backupclasses/neo4j-backup"
)

var (
	_ controller.BackupProvider               = (*Provider)(nil)
	_ controller.BackupWatcher                = (*Provider)(nil)
	_ controller.RestoreWatcher               = (*Provider)(nil)
	_ controller.BackupMirror                 = (*Provider)(nil)
	_ controller.InstanceBackupStatusReporter = (*Provider)(nil)
)

const (
	scheduleLabel                 = "neo4j.com/schedule"
	managedScheduleLabel          = "openeverest.io/managed-backup-schedule"
	neo4jScheduleCredentialPrefix = "everest-neo4j-"
)

const restoreMetadataIgnored = "restoreMetadata is deprecated and was ignored: " +
	"users, roles and privileges cannot be restored from an object-storage backup"

func defaultBackupDatabases(databases []string) []string {
	if len(databases) == 0 {
		return []string{"neo4j"}
	}
	return databases
}

func configureBackupStorage(c *controller.Context, spec *neo4jv1.Neo4jSpec) error {
	backupCfg := c.Instance().Spec.Backup
	if backupCfg == nil || !backupCfg.Enabled {
		return nil
	}
	storages, err := c.BackupStorages()
	if err != nil {
		return err
	}
	for _, storage := range storages {
		if !storage.Apply() {
			continue
		}
		credentials, err := ensureStorageCredentials(c, storage)
		if err != nil {
			c.RejectBackupStorage(storage.Name(), err)
			continue
		}
		spec.Security.CloudIdentity = &neo4jv1.CloudIdentity{StaticKeySecret: credentials}
		return nil
	}
	return nil
}

func resolveStorage(c *controller.Context, name string) (controller.ResolvedBackupStorage, error) {
	storages, err := c.BackupStorages()
	if err != nil {
		return controller.ResolvedBackupStorage{}, err
	}
	for _, storage := range storages {
		if storage.Name() == name {
			return storage, nil
		}
	}
	return controller.ResolvedBackupStorage{}, fmt.Errorf("BackupStorage %q is not registered on the Instance", name)
}

func ensureStorageCredentials(c *controller.Context, storage controller.ResolvedBackupStorage) (string, error) {
	if storage.Storage == nil {
		return "", fmt.Errorf("BackupStorage %q is missing", storage.Name())
	}
	params, _, err := c.BackupStorageS3(storage.Storage)
	if err != nil {
		return "", err
	}
	if params.EffectiveImplementation() == s3class.ImplementationGCS {
		return "", fmt.Errorf("GCS through the S3 endpoint is unsupported: %w", controller.ErrImplementationUnsupported)
	}
	source := &corev1.Secret{}
	ref := storage.Storage.Spec.Authentication.CredentialsSecretRef
	if ref == nil {
		return "", fmt.Errorf("BackupStorage %q has no credentials Secret", storage.Name())
	}
	if err := c.Get(source, ref.Name); err != nil {
		return "", fmt.Errorf("get BackupStorage credentials Secret %q: %w", ref.Name, err)
	}
	targetName := neo4jScheduleCredentialPrefix + c.Name() + "-" + storage.Name()
	target := &corev1.Secret{ObjectMeta: c.ObjectMeta(targetName)}
	target.Data = make(map[string][]byte, len(source.Data)+3)
	for key, value := range source.Data {
		target.Data[key] = append([]byte(nil), value...)
	}
	target.Data["AWS_REGION"] = []byte(params.Region)
	target.Data["AWS_DEFAULT_REGION"] = []byte(params.Region)
	if params.EndpointURL != "" {
		target.Data["AWS_ENDPOINT_URL_S3"] = []byte(params.EndpointURL)
	}
	if err := c.Apply(target); err != nil {
		return "", fmt.Errorf("apply Neo4j backup credentials Secret: %w", err)
	}
	return targetName, nil
}

func backupDestination(c *controller.Context, storageName string) (neo4jv1.BackupDestination, error) {
	storage, err := resolveStorage(c, storageName)
	if err != nil {
		return neo4jv1.BackupDestination{}, err
	}
	if !storage.Apply() {
		return neo4jv1.BackupDestination{}, fmt.Errorf("BackupStorage %q is not ready: %s", storageName, storage.Message)
	}
	credentials, err := ensureStorageCredentials(c, storage)
	if err != nil {
		return neo4jv1.BackupDestination{}, err
	}
	if _, _, err := c.BackupStorageS3(storage.Storage); err != nil {
		return neo4jv1.BackupDestination{}, err
	}
	return neo4jv1.BackupDestination{
		Type: neo4jv1.BackupDestinationS3,
		URL:  "s3://" + path.Join(storage.Storage.Spec.Location.ObjectStore.Bucket, storage.Path),
		Credentials: &neo4jv1.BackupCredentials{
			SecretName: credentials,
		},
	}, nil
}

func reconcileBackupSchedules(c *controller.Context, neo4j *neo4jv1.Neo4j) error {
	desired := map[string]struct{}{}
	backupCfg := c.Instance().Spec.Backup
	if backupCfg != nil && backupCfg.Enabled {
		for _, storage := range backupCfg.Storages {
			destination, err := backupDestination(c, storage.StorageRef.Name)
			if err != nil {
				c.RejectBackupStorage(storage.StorageRef.Name, err)
				continue
			}
			for i := range storage.Schedules {
				schedule := storage.Schedules[i]
				name := c.Name() + "-" + schedule.Name
				desired[name] = struct{}{}
				spec, err := backupScheduleSpec(neo4j.Name, schedule, destination)
				if err != nil {
					return err
				}
				obj := &neo4jv1.Neo4jBackupSchedule{ObjectMeta: c.ObjectMeta(name), Spec: spec}
				obj.Labels[managedScheduleLabel] = c.Name()
				if err := c.Apply(obj); err != nil {
					return fmt.Errorf("apply Neo4jBackupSchedule %q: %w", name, err)
				}
			}
		}
	}

	current := &neo4jv1.Neo4jBackupScheduleList{}
	if err := c.Client().List(c.Context(), current, client.InNamespace(c.Namespace())); err != nil {
		return fmt.Errorf("list Neo4jBackupSchedules: %w", err)
	}
	for i := range current.Items {
		obj := &current.Items[i]
		if obj.Labels[managedScheduleLabel] != c.Name() {
			continue
		}
		if _, ok := desired[obj.Name]; ok {
			continue
		}
		if err := c.Delete(obj); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete stale Neo4jBackupSchedule %q: %w", obj.Name, err)
		}
	}
	return nil
}

// backupScheduleSpec translates one schedule of the Instance. Its cron takes
// full backups, each starting a chain; the schedule's parameters add
// incremental backups to the chain and compact it once it is closed.
func backupScheduleSpec(
	neo4jName string, schedule corev1alpha1.InstanceBackupSchedule, destination neo4jv1.BackupDestination,
) (neo4jv1.Neo4jBackupScheduleSpec, error) {
	retention, err := backupRetention(schedule.Retention)
	if err != nil {
		return neo4jv1.Neo4jBackupScheduleSpec{}, err
	}
	params := neobackup.Neo4jBackupParameters{}
	_ = decodeRaw(schedule.Parameters, &params)
	spec := neo4jv1.Neo4jBackupScheduleSpec{
		Neo4jRef: neo4jv1.Neo4jRef{Name: neo4jName},
		Suspend:  !schedule.Enabled,
		Full: neo4jv1.FullCadence{
			Schedule:  schedule.Cron,
			Retention: retention,
		},
		BackupTemplate: neo4jv1.BackupTemplate{
			Databases:   defaultBackupDatabases(params.Databases),
			Destination: destination,
			Scratch:     backupScratch(params),
		},
	}
	if params.IncrementalSchedule != "" {
		spec.Incremental = &neo4jv1.IncrementalCadence{Schedule: params.IncrementalSchedule}
	}
	if params.Aggregate {
		spec.Aggregate = &neo4jv1.AggregateCadence{Enabled: true}
	}
	return spec, nil
}

func backupScratch(params neobackup.Neo4jBackupParameters) *neo4jv1.BackupScratch {
	if params.ScratchSize == "" {
		return nil
	}
	return &neo4jv1.BackupScratch{Size: params.ScratchSize}
}

func backupRetention(in *corev1alpha1.BackupScheduleRetention) (*neo4jv1.BackupRetention, error) {
	if in == nil {
		return nil, nil
	}
	switch in.Type {
	case corev1alpha1.BackupScheduleRetentionTypeCount:
		return &neo4jv1.BackupRetention{KeepLast: in.Count}, nil
	case corev1alpha1.BackupScheduleRetentionTypeTime:
		value := strings.TrimSpace(in.Duration)
		if len(value) < 2 {
			return nil, fmt.Errorf("invalid backup retention duration %q", value)
		}
		number, err := strconv.Atoi(value[:len(value)-1])
		if err != nil || number < 1 {
			return nil, fmt.Errorf("invalid backup retention duration %q", value)
		}
		switch value[len(value)-1] {
		case 'w':
			number *= 7
		case 'm':
			number *= 30
		case 'd':
		default:
			return nil, fmt.Errorf("invalid backup retention duration %q", value)
		}
		days := int32(number)
		return &neo4jv1.BackupRetention{KeepDays: &days}, nil
	default:
		return nil, fmt.Errorf("unsupported backup retention type %q", in.Type)
	}
}

func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	current := &neo4jv1.Neo4jBackup{}
	err := c.Get(current, backup.Name)
	if err != nil && !apierrors.IsNotFound(err) {
		return controller.BackupExecutionStatus{}, fmt.Errorf("get Neo4jBackup: %w", err)
	}
	created := apierrors.IsNotFound(err)
	params := neobackup.Neo4jBackupParameters{Type: "Full", Databases: []string{"neo4j"}}
	_ = decodeRaw(backup.Spec.Parameters, &params)
	if params.Type == "" {
		params.Type = "Full"
	}
	params.Databases = defaultBackupDatabases(params.Databases)
	if created {
		destination, err := backupDestination(c, backup.Spec.StorageRef.Name)
		if err != nil {
			return controller.BackupExecutionStatus{
				State: backupv1alpha1.BackupStatePending, Message: err.Error(),
			}, nil
		}
		desired := &neo4jv1.Neo4jBackup{
			ObjectMeta: metav1.ObjectMeta{Name: backup.Name, Namespace: backup.Namespace},
			Spec: neo4jv1.Neo4jBackupSpec{
				Neo4jRef:    neo4jv1.Neo4jRef{Name: c.Name()},
				Databases:   params.Databases,
				Destination: destination,
				Type:        neo4jv1.BackupType(params.Type),
				Scratch:     backupScratch(params),
			},
		}
		if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), desired, func() error {
			return controllerutil.SetControllerReference(backup, desired, c.Client().Scheme())
		}); err != nil {
			return controller.BackupExecutionStatus{}, fmt.Errorf("create Neo4jBackup: %w", err)
		}
		current = desired
	}

	out := controller.BackupExecutionStatus{
		OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
			Group: neo4jv1.GroupVersion.Group, Kind: "Neo4jBackup", Name: current.Name,
		},
	}
	switch current.Status.Phase {
	case neo4jv1.RunPhaseSucceeded:
		out.State = backupv1alpha1.BackupStateSucceeded
		out.CompletedAt = completionTime(current.Status.Artifacts)
		if size := artifactSize(current.Status.Artifacts); size > 0 {
			text := strconv.FormatInt(size, 10)
			out.Size = &text
		}
	case neo4jv1.RunPhaseFailed:
		out.State = backupv1alpha1.BackupStateFailed
		out.Message = current.Status.Message
	case neo4jv1.RunPhaseRunning:
		out.State = backupv1alpha1.BackupStateRunning
	default:
		out.State = backupv1alpha1.BackupStatePending
	}
	return out, nil
}

func completionTime(artifacts []neo4jv1.BackupArtifact) *metav1.Time {
	var latest *metav1.Time
	for i := range artifacts {
		if latest == nil || (artifacts[i].CompletedAt != nil && artifacts[i].CompletedAt.After(latest.Time)) {
			latest = artifacts[i].CompletedAt
		}
	}
	return latest
}

func artifactSize(artifacts []neo4jv1.BackupArtifact) int64 {
	var size int64
	for i := range artifacts {
		size += artifacts[i].SizeBytes
	}
	return size
}

func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	source, wait, err := sourceBackup(c, restore)
	if err != nil || wait != nil {
		if wait != nil {
			return *wait, nil
		}
		return controller.RestoreExecutionStatus{}, err
	}
	params := neobackup.Neo4jRestoreParameters{}
	_ = decodeRaw(restore.Spec.Parameters, &params)
	restoreSource := neo4jv1.RestoreSource{BackupRef: source.Name}
	if source.Status.OperatorBackupRef != nil && source.Status.OperatorBackupRef.Name != "" {
		restoreSource.BackupRef = source.Status.OperatorBackupRef.Name
	}
	if source.Spec.Origin.Type == backupv1alpha1.BackupOriginTypeExternal {
		// An imported Backup has no Neo4jBackup: restore its artifact by URL.
		var database string
		restoreSource, database, err = externalRestoreSource(c, source)
		if err != nil {
			return controller.RestoreExecutionStatus{
				State: backupv1alpha1.RestoreStateFailed, Message: err.Error(),
			}, nil
		}
		if len(params.Databases) == 0 {
			params.Databases = []string{database}
		}
		if len(params.Databases) != 1 {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStateFailed,
				Message: "an imported Backup holds one database: set at most one in parameters.databases",
			}, nil
		}
	}
	params.Databases = defaultBackupDatabases(params.Databases)
	if isDataSourceRestore(c, restore) {
		// A new Neo4j always has an empty default database, which the seed
		// has to replace.
		params.Overwrite = true
		params.ForceOffline = true
	}
	desired := &neo4jv1.Neo4jRestore{
		ObjectMeta: metav1.ObjectMeta{Name: restore.Name, Namespace: restore.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), desired, func() error {
		// The spec is immutable: leave a Neo4jRestore an older provider
		// created as it is.
		if desired.CreationTimestamp.IsZero() {
			desired.Spec = neo4jv1.Neo4jRestoreSpec{
				Neo4jRef:     neo4jv1.Neo4jRef{Name: c.Name()},
				Databases:    params.Databases,
				Overwrite:    params.Overwrite,
				ForceOffline: params.ForceOffline,
				Source:       restoreSource,
			}
		}
		return controllerutil.SetControllerReference(restore, desired, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create Neo4jRestore: %w", err)
	}
	out := controller.RestoreExecutionStatus{
		OperatorRestoreRef: &commonv1alpha1.TypedObjectRef{
			Group: neo4jv1.GroupVersion.Group, Kind: "Neo4jRestore", Name: desired.Name,
		},
	}
	if params.RestoreMetadata {
		out.Message = restoreMetadataIgnored
	}
	switch desired.Status.Phase {
	case neo4jv1.RunPhaseSucceeded:
		out.State = backupv1alpha1.RestoreStateSucceeded
		now := metav1.Now()
		out.CompletedAt = &now
	case neo4jv1.RunPhaseFailed:
		out.State = backupv1alpha1.RestoreStateFailed
		out.Message = desired.Status.Message
	case neo4jv1.RunPhaseRunning:
		out.State = backupv1alpha1.RestoreStateRunning
	default:
		out.State = backupv1alpha1.RestoreStatePending
	}
	return out, nil
}

// isDataSourceRestore tells whether the runtime created the Restore to seed the
// Instance from spec.dataSource.
func isDataSourceRestore(c *controller.Context, restore *backupv1alpha1.Restore) bool {
	return restore.Name == c.Name()+controller.DataSourceRestoreNameSuffix &&
		metav1.IsControlledBy(restore, c.Instance())
}

func sourceBackup(c *controller.Context, restore *backupv1alpha1.Restore) (*backupv1alpha1.Backup, *controller.RestoreExecutionStatus, error) {
	if restore.Spec.DataSource.Backup == nil || restore.Spec.DataSource.Backup.BackupRef.Name == "" {
		msg := controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStateFailed, Message: "restore requires a named source Backup",
		}
		return nil, &msg, nil
	}
	backup := &backupv1alpha1.Backup{}
	if err := c.Get(backup, restore.Spec.DataSource.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			msg := controller.RestoreExecutionStatus{
				State: backupv1alpha1.RestoreStatePending, Message: "waiting for source Backup",
			}
			return nil, &msg, nil
		}
		return nil, nil, fmt.Errorf("get source Backup: %w", err)
	}
	if backup.Status.State == backupv1alpha1.BackupStateFailed {
		msg := controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStateFailed, Message: "source Backup failed",
		}
		return nil, &msg, nil
	}
	if backup.Status.State != backupv1alpha1.BackupStateSucceeded {
		msg := controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStatePending, Message: "waiting for source Backup to succeed",
		}
		return nil, &msg, nil
	}
	return backup, nil, nil
}

func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	obj := &neo4jv1.Neo4jBackup{}
	err := c.Get(obj, backup.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get Neo4jBackup for cleanup: %w", err)
	}
	if backup.Spec.DeletionPolicy != backupv1alpha1.BackupDeletionPolicyRetain && obj.DeletionTimestamp.IsZero() {
		if err := deleteBackupArtifacts(c, backup, obj); err != nil {
			return false, err
		}
	}
	if obj.DeletionTimestamp.IsZero() {
		if err := c.Delete(obj); err != nil {
			return false, fmt.Errorf("delete Neo4jBackup: %w", err)
		}
	}
	return false, nil
}

func deleteBackupArtifacts(c *controller.Context, backup *backupv1alpha1.Backup, operatorBackup *neo4jv1.Neo4jBackup) error {
	storage, err := resolveStorage(c, backup.Spec.StorageRef.Name)
	if err != nil {
		return err
	}
	s3Client, err := controller.NewS3Client(c.Context(), c.Client(), storage.Storage)
	if err != nil {
		return fmt.Errorf("create S3 client for backup deletion: %w", err)
	}
	var prefixes []string
	for _, artifact := range operatorBackup.Status.Artifacts {
		if artifact.URI == "" {
			continue
		}
		uri := strings.TrimPrefix(artifact.URI, "s3://")
		bucket, key, ok := strings.Cut(uri, "/")
		if !ok || bucket == "" || key == "" || bucket != storage.Storage.Spec.Location.ObjectStore.Bucket {
			return fmt.Errorf("backup artifact URI %q does not belong to BackupStorage %q", artifact.URI, storage.Name())
		}
		prefixes = append(prefixes, key)
	}
	if len(prefixes) == 0 {
		return fmt.Errorf("Neo4jBackup %q has no object-store artifact URIs to delete", operatorBackup.Name)
	}
	for _, prefix := range prefixes {
		paginator := s3.NewListObjectsV2Paginator(s3Client, &s3.ListObjectsV2Input{
			Bucket: aws.String(storage.Storage.Spec.Location.ObjectStore.Bucket),
			Prefix: aws.String(prefix),
		})
		for paginator.HasMorePages() {
			page, err := paginator.NextPage(c.Context())
			if err != nil {
				return fmt.Errorf("list objects under %q: %w", prefix, err)
			}
			if len(page.Contents) == 0 {
				continue
			}
			objects := make([]s3types.ObjectIdentifier, 0, len(page.Contents))
			for _, item := range page.Contents {
				objects = append(objects, s3types.ObjectIdentifier{Key: item.Key})
			}
			result, err := s3Client.DeleteObjects(c.Context(), &s3.DeleteObjectsInput{
				Bucket: aws.String(storage.Storage.Spec.Location.ObjectStore.Bucket),
				Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)},
			})
			if err != nil {
				return fmt.Errorf("delete backup objects under %q: %w", prefix, err)
			}
			if len(result.Errors) > 0 {
				return fmt.Errorf("delete backup objects under %q: S3 returned %d errors", prefix, len(result.Errors))
			}
		}
	}
	return nil
}

func (p *Provider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	obj := &neo4jv1.Neo4jRestore{}
	err := c.Get(obj, restore.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get Neo4jRestore for cleanup: %w", err)
	}
	if obj.DeletionTimestamp.IsZero() {
		if err := c.Delete(obj); err != nil {
			return false, fmt.Errorf("delete Neo4jRestore: %w", err)
		}
	}
	return false, nil
}

func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchExternal(&neo4jv1.Neo4jBackup{}, &handler.EnqueueRequestForObject{}),
	}
}

func (p *Provider) RestoreWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchExternal(&neo4jv1.Neo4jRestore{}, &handler.EnqueueRequestForObject{}),
	}
}

func (p *Provider) OperatorBackupType() client.Object { return &neo4jv1.Neo4jBackup{} }

func (p *Provider) Mirror(ctx context.Context, c client.Client, obj client.Object) (*backupv1alpha1.Backup, error) {
	operatorBackup, ok := obj.(*neo4jv1.Neo4jBackup)
	if !ok {
		return nil, nil
	}
	scheduleName := operatorBackup.Labels[scheduleLabel]
	if scheduleName == "" {
		return nil, nil
	}
	instanceName := operatorBackup.Spec.Neo4jRef.Name
	instance := &corev1alpha1.Instance{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: operatorBackup.Namespace, Name: instanceName}, instance); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	if instance.Spec.ProviderRef.Name != p.Name() || instance.Spec.Backup == nil {
		return nil, nil
	}
	var storageName string
	var scheduleNameInSpec string
	for _, storage := range instance.Spec.Backup.Storages {
		for _, schedule := range storage.Schedules {
			if instanceName+"-"+schedule.Name == scheduleName {
				storageName = storage.StorageRef.Name
				scheduleNameInSpec = schedule.Name
				break
			}
		}
	}
	if storageName == "" {
		return nil, nil
	}
	return &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: operatorBackup.Name, Namespace: operatorBackup.Namespace},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type: backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{
					Name: instanceName,
				},
			},
			ClassRef:     instance.Spec.Backup.ClassRef,
			StorageRef:   commonv1alpha1.ObjectRef{Name: storageName},
			ScheduleName: scheduleNameInSpec,
		},
	}, nil
}

func (p *Provider) BackupStorageStatuses(c *controller.Context) ([]corev1alpha1.InstanceBackupStorageStatus, error) {
	cfg := c.Instance().Spec.Backup
	if cfg == nil {
		return nil, nil
	}
	out := make([]corev1alpha1.InstanceBackupStorageStatus, 0, len(cfg.Storages))
	resolved, err := c.BackupStorages()
	if err != nil {
		return nil, err
	}
	for _, storage := range resolved {
		out = append(out, corev1alpha1.InstanceBackupStorageStatus{
			Name: storage.Name(), Location: storage.Path,
		})
	}
	return out, nil
}

func enqueueRestoreInstance() func(context.Context, client.Object) []reconcile.Request {
	return func(_ context.Context, obj client.Object) []reconcile.Request {
		restore, ok := obj.(*backupv1alpha1.Restore)
		if !ok || restore.Spec.InstanceRef.Name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: restore.Namespace, Name: restore.Spec.InstanceRef.Name,
		}}}
	}
}

type backupRestoreType = backupv1alpha1.Restore
