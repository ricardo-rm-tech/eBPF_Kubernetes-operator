package action

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type MigrateStorageAction struct {
	Client             client.Client
	TargetStorageClass string
}

func NewMigrateStorageAction(c client.Client, targetSC string) *MigrateStorageAction {
	return &MigrateStorageAction{Client: c, TargetStorageClass: targetSC}
}

func (a *MigrateStorageAction) Execute(ctx context.Context, podName string, namespace string) error {
	log := logf.FromContext(ctx)

	if a.TargetStorageClass == "" {
		return fmt.Errorf("target storage class is empty")
	}

	// Verify the target StorageClass exists before doing anything destructive.
	var sc storagev1.StorageClass
	if err := a.Client.Get(ctx, types.NamespacedName{Name: a.TargetStorageClass}, &sc); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("target storage class %q does not exist", a.TargetStorageClass)
		}
		return fmt.Errorf("failed to verify target storage class: %w", err)
	}

	var pod corev1.Pod
	if err := a.Client.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &pod); err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	deploymentName, err := findDeploymentOwner(ctx, a.Client, &pod)
	if err != nil {
		return err
	}

	var deployment appsv1.Deployment
	if err := a.Client.Get(ctx, types.NamespacedName{Name: deploymentName, Namespace: namespace}, &deployment); err != nil {
		return fmt.Errorf("failed to get deployment: %w", err)
	}

	// Collect every PVC volume in the pod template — we update them all rather
	// than only the first match.
	type pvcRef struct {
		volIndex int
		oldName  string
	}
	var refs []pvcRef
	for i, vol := range deployment.Spec.Template.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			refs = append(refs, pvcRef{volIndex: i, oldName: vol.PersistentVolumeClaim.ClaimName})
		}
	}
	if len(refs) == 0 {
		return fmt.Errorf("no PVC found in deployment %s", deploymentName)
	}

	originalDeployment := deployment.DeepCopy()
	stamp := time.Now().UnixNano()

	for idx, ref := range refs {
		var oldPVC corev1.PersistentVolumeClaim
		if err := a.Client.Get(ctx, types.NamespacedName{Name: ref.oldName, Namespace: namespace}, &oldPVC); err != nil {
			return fmt.Errorf("failed to get old PVC %s: %w", ref.oldName, err)
		}

		if oldPVC.Spec.StorageClassName != nil && *oldPVC.Spec.StorageClassName == a.TargetStorageClass {
			log.Info("PVC is already using target storage class, skipping", "pvc", ref.oldName)
			continue
		}

		// Safety: this action does NOT copy data. Creating a fresh PVC against
		// a non-empty source is data-destructive — refuse and let the operator
		// fall back to another action.
		if !pvcLooksEmpty(&oldPVC) {
			return fmt.Errorf("PVC %s appears to hold data; refusing to migrate without a VolumeSnapshot/clone source", ref.oldName)
		}

		newPVCName := fmt.Sprintf("%s-migrated-%d-%d", ref.oldName, stamp, idx)
		dataSource := &corev1.TypedLocalObjectReference{
			APIGroup: ptrString(""),
			Kind:     "PersistentVolumeClaim",
			Name:     ref.oldName,
		}
		newPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      newPVCName,
				Namespace: namespace,
				Annotations: map[string]string{
					"autoremediation.tfg.local/migrated-from": ref.oldName,
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes:      oldPVC.Spec.AccessModes,
				Resources:        oldPVC.Spec.Resources,
				StorageClassName: &a.TargetStorageClass,
				DataSource:       dataSource,
			},
		}

		log.Info("Creating new PVC for migration", "newPVC", newPVCName, "oldPVC", ref.oldName)
		if err := a.Client.Create(ctx, newPVC); err != nil {
			return fmt.Errorf("failed to create new PVC: %w", err)
		}

		deployment.Spec.Template.Spec.Volumes[ref.volIndex].PersistentVolumeClaim.ClaimName = newPVCName
	}

	log.Info("Updating Deployment to use new PVCs", "deployment", deploymentName)
	if err := a.Client.Patch(ctx, &deployment, client.MergeFrom(originalDeployment)); err != nil {
		return fmt.Errorf("failed to update deployment: %w", err)
	}

	return nil
}

func findDeploymentOwner(ctx context.Context, c client.Client, pod *corev1.Pod) (string, error) {
	var rsName string
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "ReplicaSet" {
			rsName = owner.Name
			break
		}
	}
	if rsName == "" {
		return "", fmt.Errorf("pod %s is not owned by a ReplicaSet (StatefulSet/DaemonSet/standalone not supported)", pod.Name)
	}

	var rs appsv1.ReplicaSet
	if err := c.Get(ctx, types.NamespacedName{Name: rsName, Namespace: pod.Namespace}, &rs); err != nil {
		return "", fmt.Errorf("failed to get replicaset: %w", err)
	}
	for _, owner := range rs.OwnerReferences {
		if owner.Kind == "Deployment" {
			return owner.Name, nil
		}
	}
	return "", fmt.Errorf("replicaset %s is not owned by a Deployment", rsName)
}

func pvcLooksEmpty(pvc *corev1.PersistentVolumeClaim) bool {
	// Heuristic: a freshly-created PVC has no Phase=Bound usage history.
	// Once bound and ever used, we conservatively refuse to wipe it.
	return pvc.Status.Phase == corev1.ClaimPending
}

func ptrString(s string) *string { return &s }
