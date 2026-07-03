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
		volName  string
		oldName  string
	}
	var refs []pvcRef
	for i, vol := range deployment.Spec.Template.Spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			refs = append(refs, pvcRef{volIndex: i, volName: vol.Name, oldName: vol.PersistentVolumeClaim.ClaimName})
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

		// Safety: esta acción NO copia datos. Solo es seguro migrar cuando el
		// volumen se usa en modo SOLO LECTURA (nadie escribe en él), porque
		// entonces re-aprovisionar el PVC en la clase destino no pierde
		// escrituras en curso. Si el volumen es escribible, abortamos y dejamos
		// que el operador use otra acción de fallback.
		if !pvcUsedReadOnly(&oldPVC, &deployment, ref.volName) {
			return fmt.Errorf("PVC %s se usa en modo lectura/escritura; se aborta la migración para no perder datos (solo se migran volúmenes de solo lectura)", ref.oldName)
		}

		newPVCName := fmt.Sprintf("%s-migrated-%d-%d", ref.oldName, stamp, idx)
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

// pvcUsedReadOnly determina si es SEGURO migrar un PVC porque se usa en modo
// solo lectura durante la ejecución normal (nadie escribe en él). Devuelve true si:
//   - el PVC declara ReadOnlyMany y no permite escritura (RWO/RWX), o
//   - todos los volumeMounts de los containers principales que montan ese
//     volumen tienen readOnly: true.
//
// NOTA: los initContainers se ignoran a propósito. Un initContainer puede montar
// el volumen escribible para precargar datos una sola vez antes del arranque;
// eso no implica que la carga en ejecución escriba en él. Lo relevante para la
// seguridad de la migración es el uso en runtime (containers principales).
//
// Si el volumen se monta como escribible en algún container principal, devuelve false.
func pvcUsedReadOnly(pvc *corev1.PersistentVolumeClaim, deployment *appsv1.Deployment, volName string) bool {
	// 1) A nivel de PVC: ReadOnlyMany sin ningún modo de escritura.
	writable := false
	readOnlyMany := false
	for _, m := range pvc.Spec.AccessModes {
		switch m {
		case corev1.ReadWriteOnce, corev1.ReadWriteMany, corev1.ReadWriteOncePod:
			writable = true
		case corev1.ReadOnlyMany:
			readOnlyMany = true
		}
	}
	if readOnlyMany && !writable {
		return true
	}

	// 2) A nivel de pod: todos los mounts de este volumen en los containers
	//    principales son readOnly (los initContainers se ignoran, ver NOTA).
	mounted := false
	for _, c := range deployment.Spec.Template.Spec.Containers {
		for _, vm := range c.VolumeMounts {
			if vm.Name == volName {
				mounted = true
				if !vm.ReadOnly {
					return false // se monta escribible en un container principal
				}
			}
		}
	}

	// Solo lo consideramos seguro si realmente se monta (y siempre readOnly).
	return mounted
}
