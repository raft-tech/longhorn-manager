package controller

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/longhorn/backupstore"

	"github.com/longhorn/longhorn-manager/datastore"
	"github.com/longhorn/longhorn-manager/engineapi"
	"github.com/longhorn/longhorn-manager/types"

	longhorn "github.com/longhorn/longhorn-manager/k8s/pkg/apis/longhorn/v1beta1"
)

const (
	BackupStatusQueryInterval = 2 * time.Second
)

type BackupController struct {
	*baseController

	// which namespace controller is running with
	namespace string
	// use as the OwnerID of the controller
	controllerID string

	kubeClient    clientset.Interface
	eventRecorder record.EventRecorder

	ds *datastore.DataStore

	cacheSyncs []cache.InformerSynced
}

func NewBackupController(
	logger logrus.FieldLogger,
	ds *datastore.DataStore,
	scheme *runtime.Scheme,
	kubeClient clientset.Interface,
	controllerID string,
	namespace string) *BackupController {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(logrus.Infof)
	// TODO: remove the wrapper when every clients have moved to use the clientset.
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{
		Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events(""),
	})

	bc := &BackupController{
		baseController: newBaseController("longhorn-backup", logger),

		namespace:    namespace,
		controllerID: controllerID,

		ds: ds,

		kubeClient:    kubeClient,
		eventRecorder: eventBroadcaster.NewRecorder(scheme, v1.EventSource{Component: "longhorn-backup-controller"}),
	}

	ds.BackupInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    bc.enqueueBackup,
		UpdateFunc: func(old, cur interface{}) { bc.enqueueBackup(cur) },
		DeleteFunc: bc.enqueueBackup,
	})
	bc.cacheSyncs = append(bc.cacheSyncs, ds.BackupInformer.HasSynced)

	return bc
}

func (bc *BackupController) enqueueBackup(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("couldn't get key for object %#v: %v", obj, err))
		return
	}

	bc.queue.AddRateLimited(key)
}

func (bc *BackupController) Run(workers int, stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer bc.queue.ShutDown()

	bc.logger.Infof("Start Longhorn Backup controller")
	defer bc.logger.Infof("Shutting down Longhorn Backup controller")

	if !cache.WaitForNamedCacheSync(bc.name, stopCh, bc.cacheSyncs...) {
		return
	}
	for i := 0; i < workers; i++ {
		go wait.Until(bc.worker, time.Second, stopCh)
	}
	<-stopCh
}

func (bc *BackupController) worker() {
	for bc.processNextWorkItem() {
	}
}

func (bc *BackupController) processNextWorkItem() bool {
	key, quit := bc.queue.Get()
	if quit {
		return false
	}
	defer bc.queue.Done(key)
	err := bc.syncHandler(key.(string))
	bc.handleErr(err, key)
	return true
}

func (bc *BackupController) handleErr(err error, key interface{}) {
	if err == nil {
		bc.queue.Forget(key)
		return
	}

	if bc.queue.NumRequeues(key) < maxRetries {
		bc.logger.WithError(err).Warnf("Error syncing Longhorn backup %v", key)
		bc.queue.AddRateLimited(key)
		return
	}

	utilruntime.HandleError(err)
	bc.logger.WithError(err).Warnf("Dropping Longhorn backup %v out of the queue", key)
	bc.queue.Forget(key)
}

func (bc *BackupController) syncHandler(key string) (err error) {
	defer func() {
		err = errors.Wrapf(err, "%v: fail to sync backup %v", bc.name, key)
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	if namespace != bc.namespace {
		return nil
	}
	return bc.reconcile(name)
}

func getLoggerForBackup(logger logrus.FieldLogger, backup *longhorn.Backup) *logrus.Entry {
	return logger.WithFields(
		logrus.Fields{
			"backup": backup.Name,
		},
	)
}

func (bc *BackupController) reconcile(backupName string) (err error) {
	// Get Backup CR
	backup, err := bc.ds.GetBackup(backupName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	// Check the responsible node
	defaultEngineImage, err := bc.ds.GetSettingValueExisted(types.SettingNameDefaultEngineImage)
	if err != nil {
		return err
	}
	isResponsible, err := bc.isResponsibleFor(backup, defaultEngineImage)
	if err != nil {
		return nil
	}
	if !isResponsible {
		return nil
	}
	if backup.Status.OwnerID != bc.controllerID {
		backup.Status.OwnerID = bc.controllerID
		backup, err = bc.ds.UpdateBackupStatus(backup)
		if err != nil {
			// we don't mind others coming first
			if apierrors.IsConflict(errors.Cause(err)) {
				return nil
			}
			return err
		}
	}

	log := getLoggerForBackup(bc.logger, backup)

	// Get default backup target
	backupTarget, err := bc.ds.GetBackupTargetRO(types.DefaultBackupTargetName)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		log.Warnf("Cannot found the %s backup target", types.DefaultBackupTargetName)
		return nil
	}

	// Find the backup volume name from label
	backupVolumeName, err := bc.getBackupVolumeName(backup)
	if err != nil {
		if types.ErrorIsNotFound(err) {
			return nil // Ignore error to prevent enqueue
		}
		log.WithError(err).Warning("Cannot find backup volume name")
		return err
	}

	// Examine DeletionTimestamp to determine if object is under deletion
	if !backup.DeletionTimestamp.IsZero() {
		backupVolume, err := bc.ds.GetBackupVolume(backupVolumeName)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}

		if backupTarget.Spec.BackupTargetURL != "" &&
			backupVolume != nil && backupVolume.DeletionTimestamp == nil {
			// Initialize a backup target client
			backupTargetClient, err := getBackupTargetClient(bc.ds, backupTarget)
			if err != nil {
				log.WithError(err).Error("Error init backup target client")
				return nil // Ignore error to prevent enqueue
			}

			backupURL := backupstore.EncodeBackupURL(backup.Name, backupVolumeName, backupTargetClient.URL)
			if err := backupTargetClient.DeleteBackup(backupURL); err != nil {
				log.WithError(err).Error("Error deleting remote backup")
				return err
			}
		}

		// Request backup_volume_controller to reconcile BackupVolume immediately if it's the last backup
		if backupVolume != nil && backupVolume.Status.LastBackupName == backup.Name {
			backupVolume.Spec.SyncRequestedAt = metav1.Time{Time: time.Now().UTC()}
			if _, err = bc.ds.UpdateBackupVolume(backupVolume); err != nil && !apierrors.IsConflict(errors.Cause(err)) {
				log.WithError(err).Errorf("Error updating backup volume %s spec", backupVolumeName)
				// Do not return err to enqueue since backup_controller is responsible to
				// reconcile Backup CR spec, waits the backup_volume_controller next reconcile time
				// to update it's BackupVolume CR status
			}
		}
		return bc.ds.RemoveFinalizerForBackup(backup)
	}

	syncTime := metav1.Time{Time: time.Now().UTC()}
	existingBackup := backup.DeepCopy()
	defer func() {
		if err != nil {
			return
		}
		if reflect.DeepEqual(existingBackup.Status, backup.Status) {
			return
		}
		if _, err := bc.ds.UpdateBackupStatus(backup); err != nil && apierrors.IsConflict(errors.Cause(err)) {
			log.WithError(err).Debugf("Requeue %v due to conflict", backupName)
			bc.enqueueBackup(backup)
		}
	}()

	// Perform backup snapshot to remote backup target
	if backup.Spec.SnapshotName != "" && backup.Status.State == "" {
		// Initialize a backup target client
		backupTargetClient, err := getBackupTargetClient(bc.ds, backupTarget)
		if err != nil {
			log.WithError(err).Error("Error init backup target client")
			return nil // Ignore error to prevent enqueue
		}

		// Initialize a engine client
		engine, err := bc.ds.GetVolumeCurrentEngine(backupVolumeName)
		if err != nil {
			return err
		}
		engineCollection := &engineapi.EngineCollection{}
		engineClient, err := GetClientForEngine(engine, engineCollection, engine.Status.CurrentImage)
		if err != nil {
			return err
		}

		if err := bc.backupCreation(log, engineClient, backupTargetClient.URL, backupTargetClient.Credential, backup); err != nil {
			return err
		}
		return nil
	}

	// The backup config had synced
	if !backup.Status.LastSyncedAt.IsZero() &&
		!backup.Spec.SyncRequestedAt.After(backup.Status.LastSyncedAt.Time) {
		return nil
	}

	// Initialize a backup target client
	backupTargetClient, err := getBackupTargetClient(bc.ds, backupTarget)
	if err != nil {
		log.WithError(err).Error("Error init a backup target client")
		return nil // Ignore error to prevent enqueue
	}

	backupURL := backupstore.EncodeBackupURL(backup.Name, backupVolumeName, backupTargetClient.URL)
	backupInfo, err := backupTargetClient.InspectBackupConfig(backupURL)
	if err != nil {
		if !strings.Contains(err.Error(), "in progress") {
			log.WithError(err).Error("Error inspecting backup config")
		}
		return nil // Ignore error to prevent enqueue
	}
	if backupInfo == nil {
		return nil
	}

	// Update Backup CR status
	backup.Status.State = longhorn.BackupStateCompleted
	backup.Status.URL = backupInfo.URL
	backup.Status.SnapshotName = backupInfo.SnapshotName
	backup.Status.SnapshotCreatedAt = backupInfo.SnapshotCreated
	backup.Status.BackupCreatedAt = backupInfo.Created
	backup.Status.Size = backupInfo.Size
	backup.Status.Labels = backupInfo.Labels
	backup.Status.Messages = backupInfo.Messages
	backup.Status.VolumeName = backupInfo.VolumeName
	backup.Status.VolumeSize = backupInfo.VolumeSize
	backup.Status.VolumeCreated = backupInfo.VolumeCreated
	backup.Status.VolumeBackingImageName = backupInfo.VolumeBackingImageName
	backup.Status.LastSyncedAt = syncTime
	return nil
}

func (bc *BackupController) isResponsibleFor(b *longhorn.Backup, defaultEngineImage string) (bool, error) {
	var err error
	defer func() {
		err = errors.Wrap(err, "error while checking isResponsibleFor")
	}()

	isResponsible := isControllerResponsibleFor(bc.controllerID, bc.ds, b.Name, "", b.Status.OwnerID)

	readyNodesWithReadyEI, err := bc.ds.ListReadyNodesWithReadyEngineImage(defaultEngineImage)
	if err != nil {
		return false, err
	}
	// No node in the system has the default engine image in ready state
	if len(readyNodesWithReadyEI) == 0 {
		return false, nil
	}

	currentOwnerEngineAvailable, err := bc.ds.CheckEngineImageReadiness(defaultEngineImage, b.Status.OwnerID)
	if err != nil {
		return false, err
	}
	currentNodeEngineAvailable, err := bc.ds.CheckEngineImageReadiness(defaultEngineImage, bc.controllerID)
	if err != nil {
		return false, err
	}

	isPreferredOwner := currentNodeEngineAvailable && isResponsible
	continueToBeOwner := currentNodeEngineAvailable && bc.controllerID == b.Status.OwnerID
	requiresNewOwner := currentNodeEngineAvailable && !currentOwnerEngineAvailable

	return isPreferredOwner || continueToBeOwner || requiresNewOwner, nil
}

func (bc *BackupController) getBackupVolumeName(backup *longhorn.Backup) (string, error) {
	backupVolumeName, ok := backup.Labels[types.LonghornLabelBackupVolume]
	if !ok {
		return "", fmt.Errorf("cannot find the backup volume label")
	}
	return backupVolumeName, nil
}

func (bc *BackupController) backupCreation(log logrus.FieldLogger, engineClient engineapi.EngineClient, url string, credential map[string]string, backup *longhorn.Backup) error {
	volumeName := engineClient.Name()

	log = log.WithFields(
		logrus.Fields{
			"vol":      volumeName,
			"snapshot": backup.Spec.SnapshotName,
			"label":    backup.Spec.Labels,
		},
	)

	event := func(err error, state longhorn.BackupState, backup *longhorn.Backup, volume *longhorn.Volume) {
		if err != nil {
			bc.eventRecorder.Eventf(volume, corev1.EventTypeWarning, string(state),
				"Snapshot %s backup %s label %v: %v", backup.Spec.SnapshotName, backup.Name, backup.Spec.Labels, err)
			return
		}
		bc.eventRecorder.Eventf(volume, corev1.EventTypeNormal, string(state),
			"Snapshot %s backup %s label %v", backup.Spec.SnapshotName, backup.Name, backup.Spec.Labels)
	}

	// Get the volume CR
	volume, err := bc.ds.GetVolume(volumeName)
	if err != nil {
		return err
	}

	// Backing image validation
	biName := volume.Spec.BackingImage
	biChecksum := ""
	if biName != "" {
		bi, err := bc.ds.GetBackingImage(biName)
		if err != nil {
			return err
		}
		bv, err := bc.ds.GetBackupVolumeRO(volumeName)
		if err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if bv != nil &&
			bv.Status.BackingImageChecksum != "" && bi.Status.Checksum != "" &&
			bv.Status.BackingImageChecksum != bi.Status.Checksum {
			return fmt.Errorf("the backing image %v checksum %v in the backup volume doesn't match the current checksum %v", biName, bv.Status.BackingImageChecksum, bi.Status.Checksum)
		}
		biChecksum = bi.Status.Checksum
	}

	backup.Status.State = longhorn.BackupStateInProgress
	event(nil, backup.Status.State, backup, volume)

	go func() {
		state := backup.Status.State
		defer func() {
			backup, err := bc.ds.GetBackup(backup.Name)
			if err != nil {
				log.WithError(err).Errorf("Error get backup")
				return
			}
			existingBackup := backup.DeepCopy()

			backup.Status.State = state
			if reflect.DeepEqual(existingBackup.Status, backup.Status) {
				return
			}
			if _, err := bc.ds.UpdateBackupStatus(backup); err != nil && !apierrors.IsConflict(errors.Cause(err)) {
				log.WithError(err).Errorf("Error updating backup status")
				return
			}
		}()

		if _, err = engineClient.SnapshotBackup(backup.Name, backup.Spec.SnapshotName, url, biName, biChecksum, backup.Spec.Labels, credential); err != nil {
			state = longhorn.BackupStateError
			event(err, state, backup, volume)
			return
		}

		// Monitor snapshot backup progress
		for {
			engines, err := bc.ds.ListVolumeEngines(volume.Name)
			if err != nil {
				state = longhorn.BackupStateUnknown
				event(err, state, backup, volume)
				return
			}

			bks := &longhorn.BackupStatus{}
			for _, e := range engines {
				backupStatusList := e.Status.BackupStatus
				for _, b := range backupStatusList {
					if b.SnapshotName == backup.Spec.SnapshotName {
						bks = b
						break
					}
				}
			}
			if bks == nil {
				state = longhorn.BackupStateUnknown
				event(err, state, backup, volume)
				return
			}
			if bks.Error != "" {
				state = longhorn.BackupStateError
				event(errors.New(bks.Error), state, backup, volume)
				return
			}

			if bks.Progress != 100 {
				time.Sleep(BackupStatusQueryInterval)
				continue
			}

			// TODO:
			//   use resource monitoring https://github.com/longhorn/longhorn/issues/2441
			//   to trigger updates backup volume to run reconcile immediately
			state = longhorn.BackupStateCompleted
			event(nil, state, backup, volume)

			syncTime := metav1.Time{Time: time.Now().UTC()}
			backupVolume, err := bc.ds.GetBackupVolume(volumeName)
			if err == nil {
				// Request backup_volume_controller to reconcile BackupVolume immediately.
				backupVolume.Spec.SyncRequestedAt = syncTime
				if _, err = bc.ds.UpdateBackupVolume(backupVolume); err != nil && !apierrors.IsConflict(errors.Cause(err)) {
					log.WithError(err).Errorf("Error updating backup volume %s spec", volume.Name)
				}
			} else if err != nil && apierrors.IsNotFound(err) {
				// Request backup_target_controller to reconcile BackupTarget immediately.
				backupTarget, err := bc.ds.GetBackupTarget(types.DefaultBackupTargetName)
				if err != nil {
					log.WithError(err).Warn("Failed to get backup target")
					return
				}
				backupTarget.Spec.SyncRequestedAt = syncTime
				if _, err = bc.ds.UpdateBackupTarget(backupTarget); err != nil && !apierrors.IsConflict(errors.Cause(err)) {
					log.WithError(err).Warn("Failed to update backup target")
				}
			}
			return
		}
	}()

	return nil
}
