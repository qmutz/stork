package controllers

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	crdv1 "github.com/kubernetes-incubator/external-storage/snapshot/pkg/apis/crd/v1"
	"github.com/libopenstorage/stork/drivers/volume"
	stork_api "github.com/libopenstorage/stork/pkg/apis/stork/v1alpha1"
	"github.com/libopenstorage/stork/pkg/controllers"
	"github.com/libopenstorage/stork/pkg/k8sutils"
	"github.com/libopenstorage/stork/pkg/log"
	"github.com/libopenstorage/stork/pkg/rule"
	snapshotcontrollers "github.com/libopenstorage/stork/pkg/snapshot/controllers"
	"github.com/portworx/sched-ops/k8s/apiextensions"
	"github.com/portworx/sched-ops/k8s/core"
	k8sextops "github.com/portworx/sched-ops/k8s/externalstorage"
	storkops "github.com/portworx/sched-ops/k8s/stork"
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	apiextensionsv1beta1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1beta1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	runtimeclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	validateCRDInterval time.Duration = 5 * time.Second
	validateCRDTimeout  time.Duration = 1 * time.Minute

	updateCRD = true

	// volumeSnapshot* is configuration of exponential backoff for
	// waiting for snapshot operation to complete. Starting with 2
	// seconds, multiplying by 1.0 with each step and taking 60 steps at maximum.
	// It will time out after 120 seconds.
	volumeSnapshotInitialDelay = 2 * time.Second
	volumeSnapshotFactor       = 1
	volumeSnapshotSteps        = 60
)

var snapDeleteBackoff = wait.Backoff{
	Duration: volumeSnapshotInitialDelay,
	Factor:   volumeSnapshotFactor,
	Steps:    volumeSnapshotSteps,
}

// NewGroupSnapshot creates a new instance of GroupSnapshotController.
func NewGroupSnapshot(mgr manager.Manager, d volume.Driver, r record.EventRecorder) *GroupSnapshotController {
	return &GroupSnapshotController{
		client:    mgr.GetClient(),
		volDriver: d,
		recorder:  r,
	}
}

// GroupSnapshotController groupSnapshotcontroller
type GroupSnapshotController struct {
	client runtimeclient.Client

	volDriver           volume.Driver
	recorder            record.EventRecorder
	bgChannelsForRules  map[string]chan bool
	minResourceVersions map[string]string
}

// Init Initialize the groupSnapshot controller
func (m *GroupSnapshotController) Init(mgr manager.Manager) error {
	err := m.createCRD()
	if err != nil {
		return err
	}

	m.bgChannelsForRules = make(map[string]chan bool)
	m.minResourceVersions = make(map[string]string)

	return controllers.RegisterTo(mgr, "group-snapshot-controller", m, &stork_api.GroupVolumeSnapshot{})
}

// Reconcile reads that state of the cluster for an object and makes changes based on the state read
// and what is in the Spec.
//
// The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (m *GroupSnapshotController) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	logrus.Tracef("Reconciling GroupVolumeSnapshot %s/%s", request.Namespace, request.Name)

	// Fetch the GroupSnapshot instance
	groupSnapshot := &stork_api.GroupVolumeSnapshot{}
	err := m.client.Get(context.TODO(), request.NamespacedName, groupSnapshot)
	if err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			// Owned objects are automatically garbage collected. For additional cleanup logic use finalizers.
			// Return and don't requeue
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	if !controllers.ContainsFinalizer(groupSnapshot, controllers.FinalizerCleanup) {
		controllers.SetFinalizer(groupSnapshot, controllers.FinalizerCleanup)
		return reconcile.Result{Requeue: true}, m.client.Update(context.TODO(), groupSnapshot)
	}

	if err = m.handle(context.TODO(), groupSnapshot); err != nil {
		logrus.Errorf("%s: %s/%s: %s", reflect.TypeOf(m), groupSnapshot.Namespace, groupSnapshot.Name, err)
		return reconcile.Result{RequeueAfter: controllers.DefaultRequeueError}, err
	}

	return reconcile.Result{RequeueAfter: controllers.DefaultRequeue}, nil
}

func (m *GroupSnapshotController) handle(ctx context.Context, groupSnapshot *stork_api.GroupVolumeSnapshot) error {
	if groupSnapshot.DeletionTimestamp != nil {
		if controllers.ContainsFinalizer(groupSnapshot, controllers.FinalizerCleanup) {
			if err := m.handleDelete(groupSnapshot); err != nil {
				return fmt.Errorf("cleanup: %s", err)
			}
		}

		if groupSnapshot.GetFinalizers() != nil {
			controllers.RemoveFinalizer(groupSnapshot, controllers.FinalizerCleanup)
			return m.client.Update(ctx, groupSnapshot)
		}

		return nil
	}

	var err error
	minVer, present := m.minResourceVersions[string(groupSnapshot.UID)]
	if present {
		minVersion, err := version.NewVersion(minVer)
		if err != nil {
			log.GroupSnapshotLog(groupSnapshot).Errorf("Error handling event: %v err: %v", groupSnapshot, err.Error())
			m.recorder.Event(groupSnapshot,
				v1.EventTypeWarning,
				string(stork_api.GroupSnapshotFailed),
				err.Error())
			return err
		}

		snapVersion, err := version.NewVersion(groupSnapshot.ResourceVersion)
		if err != nil {
			log.GroupSnapshotLog(groupSnapshot).Errorf("Error handling event: %v err: %v", groupSnapshot, err.Error())
			m.recorder.Event(groupSnapshot,
				v1.EventTypeWarning,
				string(stork_api.GroupSnapshotFailed),
				err.Error())
			return err
		}

		if snapVersion.LessThan(minVersion) {
			log.GroupSnapshotLog(groupSnapshot).Infof(
				"Already processed groupSnapshot version (%s) higher than: %s. Skipping event.",
				minVer, groupSnapshot.ResourceVersion)
			return nil
		}
	}

	var updateCRDForThisEvent bool
	switch groupSnapshot.Status.Stage {
	case stork_api.GroupSnapshotStageInitial,
		stork_api.GroupSnapshotStagePreChecks:
		updateCRDForThisEvent, err = m.handleInitial(groupSnapshot)
	case stork_api.GroupSnapshotStagePreSnapshot:
		var updatedGroupSnapshot *stork_api.GroupVolumeSnapshot
		updatedGroupSnapshot, updateCRDForThisEvent, err = m.handlePreSnap(groupSnapshot)
		if err == nil {
			groupSnapshot = updatedGroupSnapshot
		}
	case stork_api.GroupSnapshotStageSnapshot:
		updateCRDForThisEvent, err = m.handleSnap(groupSnapshot)

		// Terminate background commands regardless of failure if the snapshots are
		// triggered
		snapUID := string(groupSnapshot.ObjectMeta.UID)
		if areAllSnapshotsStarted(groupSnapshot.Status.VolumeSnapshots) {
			backgroundChannel, present := m.bgChannelsForRules[snapUID]
			if present {
				backgroundChannel <- true
				delete(m.bgChannelsForRules, snapUID)
			}
		}
	case stork_api.GroupSnapshotStagePostSnapshot:
		var updatedGroupSnapshot *stork_api.GroupVolumeSnapshot
		updatedGroupSnapshot, updateCRDForThisEvent, err = m.handlePostSnap(groupSnapshot)
		if err == nil {
			groupSnapshot = updatedGroupSnapshot
		}
	case stork_api.GroupSnapshotStageFinal:
		return m.handleFinal(groupSnapshot)
	default:
		err = fmt.Errorf("invalid stage for group snapshot: %v", groupSnapshot.Status.Stage)
	}

	if err != nil {
		m.recorder.Event(groupSnapshot,
			v1.EventTypeWarning,
			string(stork_api.GroupSnapshotFailed),
			err.Error())
		return err
	}

	if updateCRDForThisEvent {
		SetKind(groupSnapshot)
		updateErr := m.client.Update(context.TODO(), groupSnapshot)
		if updateErr != nil {
			return updateErr
		}

		// Since we updated, bump the minimum resource version
		// This is needed since the resync period can overlap with the time a handle
		// event is already being processed. In such situation, the operator framework
		// with provide a groupSnapshot which is the same version as the previous groupSnapshot
		// If we reprocess an outdated object, this can throw off the status checks in the snapshot stage
		m.minResourceVersions[string(groupSnapshot.UID)] = groupSnapshot.ResourceVersion
	}

	return nil
}

func (m *GroupSnapshotController) createCRD() error {
	resource := apiextensions.CustomResource{
		Name:    stork_api.GroupVolumeSnapshotResourceName,
		Plural:  stork_api.GroupVolumeSnapshotResourcePlural,
		Group:   stork_api.SchemeGroupVersion.Group,
		Version: stork_api.SchemeGroupVersion.Version,
		Scope:   apiextensionsv1beta1.NamespaceScoped,
		Kind:    reflect.TypeOf(stork_api.GroupVolumeSnapshot{}).Name(),
	}
	err := apiextensions.Instance().CreateCRD(resource)
	if err != nil && !errors.IsAlreadyExists(err) {
		return err
	}

	return apiextensions.Instance().ValidateCRD(resource, validateCRDTimeout, validateCRDInterval)
}

func (m *GroupSnapshotController) handleInitial(groupSnap *stork_api.GroupVolumeSnapshot) (bool, error) {
	var err error

	// Pre checks
	if len(groupSnap.Spec.PVCSelector.MatchExpressions) > 0 {
		err = fmt.Errorf("matchExpressions are currently not supported in the spec. Use matchLabels")
	}

	if len(groupSnap.Spec.PVCSelector.MatchLabels) == 0 {
		err = fmt.Errorf("matchLabels are required for group snapshots. Refer to spec examples")
	}

	if err != nil {
		groupSnap.Status.Status = stork_api.GroupSnapshotFailed
		groupSnap.Status.Stage = stork_api.GroupSnapshotStageFinal
		return updateCRD, err
	}

	_, err = k8sutils.GetPVCsForGroupSnapshot(groupSnap.Namespace, groupSnap.Spec.PVCSelector.MatchLabels)
	if err != nil {
		if groupSnap.Status.Status == stork_api.GroupSnapshotPending {
			return !updateCRD, err
		}

		groupSnap.Status.Status = stork_api.GroupSnapshotPending
		groupSnap.Status.Stage = stork_api.GroupSnapshotStagePreChecks
	} else {
		// Validate pre and post snap rules
		preSnapRuleName := groupSnap.Spec.PreExecRule
		if len(preSnapRuleName) > 0 {
			if _, err := storkops.Instance().GetRule(preSnapRuleName, groupSnap.Namespace); err != nil {
				return !updateCRD, err
			}
		}

		postSnapRuleName := groupSnap.Spec.PostExecRule
		if len(postSnapRuleName) > 0 {
			if _, err := storkops.Instance().GetRule(postSnapRuleName, groupSnap.Namespace); err != nil {
				return !updateCRD, err
			}
		}

		groupSnap.Status.Status = stork_api.GroupSnapshotInProgress

		if len(preSnapRuleName) > 0 {
			// done with pre-checks, move to pre-snapshot stage
			groupSnap.Status.Stage = stork_api.GroupSnapshotStagePreSnapshot
		} else {
			// No pre rule, move to snapshot stage
			groupSnap.Status.Stage = stork_api.GroupSnapshotStageSnapshot
		}
	}

	return updateCRD, err
}

func (m *GroupSnapshotController) handlePreSnap(groupSnap *stork_api.GroupVolumeSnapshot) (
	*stork_api.GroupVolumeSnapshot, bool, error) {
	ruleName := groupSnap.Spec.PreExecRule
	if len(ruleName) == 0 {
		groupSnap.Status.Status = stork_api.GroupSnapshotInProgress
		// No rule, move to snapshot stage
		groupSnap.Status.Stage = stork_api.GroupSnapshotStageSnapshot
		return groupSnap, updateCRD, nil
	}

	log.GroupSnapshotLog(groupSnap).Infof("Running pre-snapshot rule: %s", ruleName)
	r, err := storkops.Instance().GetRule(ruleName, groupSnap.Namespace)
	if err != nil {
		return nil, !updateCRD, err
	}

	backgroundCommandTermChan, err := rule.ExecuteRule(r, rule.PreExecRule, groupSnap, groupSnap.Namespace)
	if err != nil {
		if backgroundCommandTermChan != nil {
			backgroundCommandTermChan <- true // terminate background commands if running
		}

		return nil, !updateCRD, err
	}

	// refresh the latest groupSnap as ExecuteRule might have  updated it
	groupSnap, err = storkops.Instance().GetGroupSnapshot(groupSnap.GetName(), groupSnap.GetNamespace())
	if err != nil {
		return nil, !updateCRD, err
	}

	if backgroundCommandTermChan != nil {
		snapUID := string(groupSnap.ObjectMeta.UID)
		m.bgChannelsForRules[snapUID] = backgroundCommandTermChan
	}

	// done with pre-snapshot, move to snapshot stage
	groupSnap.Status.Stage = stork_api.GroupSnapshotStageSnapshot
	return groupSnap, updateCRD, nil
}

func (m *GroupSnapshotController) handleSnap(groupSnap *stork_api.GroupVolumeSnapshot) (bool, error) {
	var (
		err      error
		stage    stork_api.GroupVolumeSnapshotStageType
		status   stork_api.GroupVolumeSnapshotStatusType
		response *volume.GroupSnapshotCreateResponse
	)

	if len(groupSnap.Status.VolumeSnapshots) > 0 {
		log.GroupSnapshotLog(groupSnap).Infof("Group snapshot already active. Checking status")
		response, err = m.volDriver.GetGroupSnapshotStatus(groupSnap)
	} else {
		log.GroupSnapshotLog(groupSnap).Infof("Creating new group snapshot")
		response, err = m.volDriver.CreateGroupSnapshot(groupSnap)
	}

	if err != nil {
		return !updateCRD, err
	}

	if len(response.Snapshots) == 0 {
		err = fmt.Errorf("group snapshot call returned 0 snapshots in response from driver")
		return !updateCRD, err
	}

	if isFailed, failedTasks := isAnySnapshotFailed(response.Snapshots); isFailed {
		errMsgPrefix := fmt.Sprintf("Some snapshots in group have failed: %s", failedTasks)

		if groupSnap.Status.NumRetries < groupSnap.Spec.MaxRetries {
			groupSnap.Status.NumRetries++

			err = fmt.Errorf("%s. Resetting group snapshot for retry: %d",
				errMsgPrefix, groupSnap.Status.NumRetries)
			response.Snapshots = nil // so that snapshots are retried
			stage = stork_api.GroupSnapshotStageSnapshot
			status = stork_api.GroupSnapshotPending
		} else {
			if groupSnap.Spec.MaxRetries == 0 {
				err = fmt.Errorf("%s. Failing the groupsnapshot as retries are not enabled", errMsgPrefix)
			} else {
				err = fmt.Errorf("%s. Failing the groupsnapshot as all %d retries are exhausted",
					errMsgPrefix, groupSnap.Spec.MaxRetries)
			}

			// even though failed, we still need to run post rules
			stage = stork_api.GroupSnapshotStagePostSnapshot
			status = stork_api.GroupSnapshotFailed
		}

		log.GroupSnapshotLog(groupSnap).Errorf(err.Error())
		m.recorder.Event(groupSnap,
			v1.EventTypeWarning,
			string(stork_api.GroupSnapshotFailed),
			err.Error())
	} else if areAllSnapshotsDone(response.Snapshots) {
		log.GroupSnapshotLog(groupSnap).Infof("All snapshots in group are done")
		// Create volumesnapshot and volumesnapshotdata objects in API
		response.Snapshots, err = m.createSnapAndDataObjects(groupSnap, response.Snapshots)
		if err != nil {
			return !updateCRD, err
		}

		stage = stork_api.GroupSnapshotStagePostSnapshot
		status = stork_api.GroupSnapshotInProgress
	} else {
		log.GroupSnapshotLog(groupSnap).Infof("Some snapshots still in progress")
		stage = stork_api.GroupSnapshotStageSnapshot
		status = stork_api.GroupSnapshotInProgress
	}

	groupSnap.Status.VolumeSnapshots = response.Snapshots
	groupSnap.Status.Status = status
	groupSnap.Status.Stage = stage

	return updateCRD, nil
}

func (m *GroupSnapshotController) replaceSnapshotData(
	snapData *crdv1.VolumeSnapshotData,
) error {
	data, err := k8sextops.Instance().GetSnapshotData(snapData.Metadata.Name)
	if err != nil {
		return err
	}

	// If the existing one isn't the same as the new one delete and re-create it
	if !reflect.DeepEqual(data.Spec, snapData.Spec) {
		err := k8sextops.Instance().DeleteSnapshotData(snapData.Metadata.Name)
		if err != nil {
			return err
		}
		_, err = k8sextops.Instance().CreateSnapshotData(snapData)
		return err
	}
	return nil
}

func (m *GroupSnapshotController) replaceSnapshot(
	snap *crdv1.VolumeSnapshot,
) error {
	s, err := k8sextops.Instance().GetSnapshotData(snap.Metadata.Name)
	if err != nil {
		return err
	}

	// If the existing one isn't the same as the new one delete and re-create it
	if !reflect.DeepEqual(s.Spec, snap.Spec) {
		err := k8sextops.Instance().DeleteSnapshotData(snap.Metadata.Name)
		if err != nil {
			return err
		}
		_, err = k8sextops.Instance().CreateSnapshot(snap)
		return err
	}
	return nil
}

func (m *GroupSnapshotController) createSnapAndDataObjects(
	groupSnap *stork_api.GroupVolumeSnapshot, snapshots []*stork_api.VolumeSnapshotStatus) (
	[]*stork_api.VolumeSnapshotStatus, error) {
	updatedStatues := make([]*stork_api.VolumeSnapshotStatus, 0)

	parentName := groupSnap.GetName()
	parentNamespace := groupSnap.GetNamespace()
	if len(parentNamespace) == 0 {
		parentNamespace = metav1.NamespaceDefault
	}
	parentUUID := groupSnap.GetUID()
	snapLabels := groupSnap.GetLabels()
	snapAnnotations := groupSnap.GetAnnotations()
	createSnapObjects := make([]*crdv1.VolumeSnapshot, 0)

	if len(groupSnap.Spec.RestoreNamespaces) > 0 {
		if len(snapAnnotations) == 0 {
			snapAnnotations = make(map[string]string)
		}

		snapAnnotations[snapshotcontrollers.StorkSnapshotRestoreNamespacesAnnotation] = strings.Join(groupSnap.Spec.RestoreNamespaces, ",")
	}

	for _, snapshot := range snapshots {
		parentPVCOrVolID, err := m.getPVCNameFromVolumeID(snapshot.ParentVolumeID)
		if err != nil {
			return nil, err
		}

		volumeSnapshotName := fmt.Sprintf("%s-%s-%s", parentName, parentPVCOrVolID, parentUUID)

		var lastCondition crdv1.VolumeSnapshotDataCondition
		if snapshot.Conditions != nil && len(snapshot.Conditions) > 0 {
			conditions := snapshot.Conditions
			ind := len(conditions) - 1
			lastCondition = crdv1.VolumeSnapshotDataCondition{
				Type:    (crdv1.VolumeSnapshotDataConditionType)(conditions[ind].Type),
				Status:  conditions[ind].Status,
				Message: conditions[ind].Message,
			}
		}

		snapData := &crdv1.VolumeSnapshotData{
			Metadata: metav1.ObjectMeta{
				Name:        volumeSnapshotName,
				Labels:      snapLabels,
				Annotations: snapAnnotations,
			},
			Spec: crdv1.VolumeSnapshotDataSpec{
				VolumeSnapshotRef: &v1.ObjectReference{
					Kind:      "VolumeSnapshot",
					Name:      volumeSnapshotName,
					Namespace: parentNamespace,
				},
				PersistentVolumeRef:      &v1.ObjectReference{},
				VolumeSnapshotDataSource: *snapshot.DataSource,
			},
			Status: crdv1.VolumeSnapshotDataStatus{
				Conditions: []crdv1.VolumeSnapshotDataCondition{
					lastCondition,
				},
			},
		}

		_, err = k8sextops.Instance().CreateSnapshotData(snapData)
		if err != nil {
			// Try to replace the snapshot data if it already exists
			if errors.IsAlreadyExists(err) {
				err = m.replaceSnapshotData(snapData)
			}
			if err != nil {
				err = fmt.Errorf("error creating the VolumeSnapshotData for snap %s due to err: %v",
					volumeSnapshotName, err)
				log.GroupSnapshotLog(groupSnap).Errorf(err.Error())
				return nil, err
			}
		}

		snap := &crdv1.VolumeSnapshot{
			Metadata: metav1.ObjectMeta{
				Name:        volumeSnapshotName,
				Namespace:   parentNamespace,
				Labels:      snapLabels,
				Annotations: snapAnnotations,
				OwnerReferences: []metav1.OwnerReference{
					{
						Name:       parentName,
						UID:        parentUUID,
						Kind:       groupSnap.GetObjectKind().GroupVersionKind().Kind,
						APIVersion: groupSnap.GetObjectKind().GroupVersionKind().GroupVersion().String(),
					},
				},
			},
			Spec: crdv1.VolumeSnapshotSpec{
				SnapshotDataName:          snapData.Metadata.Name,
				PersistentVolumeClaimName: parentPVCOrVolID,
			},
			Status: crdv1.VolumeSnapshotStatus{
				Conditions: snapshot.Conditions,
			},
		}

		snap, err = k8sextops.Instance().CreateSnapshot(snap)
		if err != nil {
			// Try to replace the snapshot if it already exists
			if errors.IsAlreadyExists(err) {
				err = m.replaceSnapshot(snap)
			}

			// For other errors and if replace fails revert snapdata
			if err != nil {
				deleteErr := k8sextops.Instance().DeleteSnapshotData(snapData.Metadata.Name)
				if deleteErr != nil {
					log.GroupSnapshotLog(groupSnap).Errorf("Failed to revert volumesnapshotdata due to: %v", deleteErr)
				}

				revertSnapObjs(createSnapObjects)
				return nil, err
			}
		}

		createSnapObjects = append(createSnapObjects, snap)

		snapshot.VolumeSnapshotName = volumeSnapshotName
		updatedStatues = append(updatedStatues, snapshot)
	}

	return updatedStatues, nil
}

func revertSnapObjs(snapObjs []*crdv1.VolumeSnapshot) {
	if len(snapObjs) == 0 {
		return
	}

	failedDeletions := make(map[string]error)

	for _, snap := range snapObjs {
		err := wait.ExponentialBackoff(snapDeleteBackoff, func() (bool, error) {
			deleteErr := k8sextops.Instance().DeleteSnapshot(snap.Metadata.Name, snap.Metadata.Namespace)
			if deleteErr != nil {
				log.SnapshotLog(snap).Infof("Failed to delete volumesnapshot due to: %v", deleteErr)
				return false, nil
			}

			return true, nil
		})
		if err != nil {
			failedDeletions[fmt.Sprintf("[%s] %s", snap.Metadata.Namespace, snap.Metadata.Name)] = err
		}
	}

	if len(failedDeletions) > 0 {
		errString := ""
		for failedID, failedErr := range failedDeletions {
			errString = fmt.Sprintf("%s delete of %s failed due to err: %v.\n", errString, failedID, failedErr)
		}

		logrus.Errorf("Failed to revert created volumesnapshots. err: %s", errString)
		return
	}

	logrus.Infof("Successfully reverted volumesnapshots")
}

// this is best effort as can be vol ID if PVC is deleted
func (m *GroupSnapshotController) getPVCNameFromVolumeID(volID string) (string, error) {
	volInfo, err := m.volDriver.InspectVolume(volID)
	if err != nil {
		logrus.Warnf("Volume: %s not found due to: %v", volID, err)
		return volID, nil
	}

	parentPV, err := core.Instance().GetPersistentVolume(volInfo.VolumeName)
	if err != nil {
		logrus.Warnf("Parent PV: %s not found due to: %v", volInfo.VolumeName, err)
		return volID, nil
	}

	pvc, err := core.Instance().GetPersistentVolumeClaim(parentPV.Spec.ClaimRef.Name, parentPV.Spec.ClaimRef.Namespace)
	if err != nil {
		return volID, nil
	}

	return pvc.GetName(), nil

}

func (m *GroupSnapshotController) handlePostSnap(groupSnap *stork_api.GroupVolumeSnapshot) (
	*stork_api.GroupVolumeSnapshot, bool, error) {
	ruleName := groupSnap.Spec.PostExecRule
	if len(ruleName) == 0 { // No rule, move to final stage
		if groupSnap.Status.Status != stork_api.GroupSnapshotFailed {
			groupSnap.Status.Status = stork_api.GroupSnapshotSuccessful
		}
		groupSnap.Status.Stage = stork_api.GroupSnapshotStageFinal
		return groupSnap, updateCRD, nil
	}

	logrus.Infof("Running post-snapshot rule: %s", ruleName)
	r, err := storkops.Instance().GetRule(ruleName, groupSnap.Namespace)
	if err != nil {
		return nil, !updateCRD, err
	}

	_, err = rule.ExecuteRule(r, rule.PostExecRule, groupSnap, groupSnap.Namespace)
	if err != nil {
		return nil, !updateCRD, err
	}

	// refresh the latest groupSnap as ExecuteRule might have  updated it
	groupSnap, err = storkops.Instance().GetGroupSnapshot(groupSnap.GetName(), groupSnap.GetNamespace())
	if err != nil {
		return nil, !updateCRD, err
	}

	// done with post-snapshot, move to final stage
	if groupSnap.Status.Status != stork_api.GroupSnapshotFailed {
		groupSnap.Status.Status = stork_api.GroupSnapshotSuccessful
	}
	groupSnap.Status.Stage = stork_api.GroupSnapshotStageFinal
	return groupSnap, updateCRD, nil
}

func (m *GroupSnapshotController) handleFinal(groupSnap *stork_api.GroupVolumeSnapshot) error {
	// Check if user has updated restore namespace
	childSnapshots := groupSnap.Status.VolumeSnapshots
	if len(childSnapshots) > 0 {
		currentRestoreNamespaces := ""
		latestRestoreNamespacesInCSV := strings.Join(groupSnap.Spec.RestoreNamespaces, ",")

		vsObject, err := k8sextops.Instance().GetSnapshot(childSnapshots[0].VolumeSnapshotName, groupSnap.GetNamespace())
		if err != nil {
			return err
		}

		childSnapAnnotations := vsObject.Metadata.Annotations
		if childSnapAnnotations != nil {
			currentRestoreNamespaces = childSnapAnnotations[snapshotcontrollers.StorkSnapshotRestoreNamespacesAnnotation]
		}

		if latestRestoreNamespacesInCSV != currentRestoreNamespaces {
			log.GroupSnapshotLog(groupSnap).Infof("Updating restore namespaces for groupsnapshot to: %s",
				latestRestoreNamespacesInCSV)
			for _, childSnap := range childSnapshots {
				vs, err := k8sextops.Instance().GetSnapshot(childSnap.VolumeSnapshotName, groupSnap.GetNamespace())
				if err != nil {
					if errors.IsNotFound(err) {
						continue
					}

					return err
				}

				if vs.Metadata.Annotations == nil {
					vs.Metadata.Annotations = make(map[string]string)
				}

				vs.Metadata.Annotations[snapshotcontrollers.StorkSnapshotRestoreNamespacesAnnotation] = latestRestoreNamespacesInCSV
				_, err = k8sextops.Instance().UpdateSnapshot(vs)
				if err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (m *GroupSnapshotController) handleDelete(groupSnap *stork_api.GroupVolumeSnapshot) error {
	// no need to track minResourceVersion for this group snap any longer
	delete(m.minResourceVersions, string(groupSnap.UID))

	if err := m.volDriver.DeleteGroupSnapshot(groupSnap); err != nil {
		return err
	}

	return nil
}

// isAnySnapshotFailed checks if any of the given snapshots is in error state and returns
// task IDs of failed snapshots
func isAnySnapshotFailed(snapshots []*stork_api.VolumeSnapshotStatus) (bool, []string) {
	failedTasks := make([]string, 0)
	for _, snapshot := range snapshots {
		conditions := snapshot.Conditions
		if len(conditions) > 0 {
			lastCondition := conditions[0]
			if lastCondition.Status == v1.ConditionTrue && lastCondition.Type == crdv1.VolumeSnapshotConditionError {
				failedTasks = append(failedTasks, snapshot.TaskID)
			}
		}
	}

	return len(failedTasks) > 0, failedTasks
}

func areAllSnapshotsStarted(snapshots []*stork_api.VolumeSnapshotStatus) bool {
	if len(snapshots) == 0 {
		return false
	}

	for _, snapshot := range snapshots {
		if len(snapshot.Conditions) == 0 {
			// no conditions so assuming not started as rest all conditions indicate the
			// snapshot is either terminal (done, failed) or active.
			return false
		}
	}

	return true
}

func areAllSnapshotsDone(snapshots []*stork_api.VolumeSnapshotStatus) bool {
	if len(snapshots) == 0 {
		return false
	}

	readySnapshots := 0
	for _, snapshot := range snapshots {
		conditions := snapshot.Conditions
		if len(conditions) > 0 {
			lastCondition := conditions[0]
			if lastCondition.Status == v1.ConditionTrue && lastCondition.Type == crdv1.VolumeSnapshotConditionReady {
				readySnapshots++
			}
		}
	}

	return readySnapshots == len(snapshots)
}

// SetKind sets the group snapshopt kind
func SetKind(snap *stork_api.GroupVolumeSnapshot) {
	snap.Kind = "GroupVolumeSnapshot"
	snap.APIVersion = stork_api.SchemeGroupVersion.String()
}
