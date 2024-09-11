package storagecluster

import (
	"fmt"
	rookCephv1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1"
	"k8s.io/apimachinery/pkg/types"
	"strings"
	"sync"

	operatorsv1alpha1 "github.com/operator-framework/api/pkg/operators/v1alpha1"
	ocsv1 "github.com/red-hat-storage/ocs-operator/api/v4/v1"
	ocsv1a1 "github.com/red-hat-storage/ocs-operator/api/v4/v1alpha1"
	"github.com/red-hat-storage/ocs-operator/v4/controllers/util"
	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	odfInfoKeySuffix          = "config.yaml"
	odfDeploymentTypeExternal = "external"
	odfDeploymentTypeInternal = "internal"
	rookCephMonSecretName     = "rook-ceph-mon"
	fsidKey                   = "fsid"
	ocsOperatorNamePrefix     = "ocs-operator"
	OdfInfoConfigMapName      = "odf-info"
	odfInfoMapKind            = "ConfigMap"
)

type odfInfoConfig struct{}

var _ resourceManager = &odfInfoConfig{}

var mutex sync.RWMutex

var testSkipIsDrOptimized = false

// ensureCreated ensures that a ConfigMap resource exists with its Spec in
// the desired state.
func (obj *odfInfoConfig) ensureCreated(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) (reconcile.Result, error) {
	operatorNamespace, err := util.GetOperatorNamespace()
	if err != nil {
		return reconcile.Result{}, err
	}

	odfInfoConfigMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      OdfInfoConfigMapName,
			Namespace: operatorNamespace,
		},
	}

	mutex.Lock()
	defer mutex.Unlock()
	_, err = ctrl.CreateOrUpdate(r.ctx, r.Client, odfInfoConfigMap, func() error {
		// Note: purposely setting OwnerRef instead of ControllerRef, which alongside MatchEveryOwner
		// sent in to OwnsOptions in the ConfigMap Owns, guarantees relevant events will be triggered
		if err = controllerutil.SetOwnerReference(storageCluster, odfInfoConfigMap, r.Scheme); err != nil {
			return err
		}
		r.Log.Info("Creating or updating odf-info config map", odfInfoMapKind, client.ObjectKeyFromObject(odfInfoConfigMap))
		odfInfoKey := obj.getOdfInfoKeyName(storageCluster)

		odfInfoData, configErr := getOdfInfoData(r, storageCluster)
		if configErr != nil {
			return fmt.Errorf("failed to get odf-info config map data: %v", configErr)
		}
		if odfInfoConfigMap.Data == nil {
			odfInfoConfigMap.Data = map[string]string{}
		}
		// Creates or appends to the data map
		odfInfoConfigMap.Data[odfInfoKey] = odfInfoData
		return nil
	})
	if err != nil {
		r.Log.Error(err, "failed to create or update odf-info config map", odfInfoMapKind, client.ObjectKeyFromObject(odfInfoConfigMap))
		return reconcile.Result{}, fmt.Errorf("failed to create or update odf-info config: %v", err)
	}
	return reconcile.Result{}, nil
}

func (obj *odfInfoConfig) getOdfInfoKeyName(storageCluster *ocsv1.StorageCluster) string {
	return fmt.Sprintf("%s_%s.%s", storageCluster.Namespace, storageCluster.Name, odfInfoKeySuffix)
}

// ensureDeleted is dummy func for the odfInfoConfig
func (obj *odfInfoConfig) ensureDeleted(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) (reconcile.Result, error) {
	operatorNamespace, err := util.GetOperatorNamespace()
	if err != nil {
		return reconcile.Result{}, err
	}
	odfInfoConfigMap := &corev1.ConfigMap{}
	odfInfoConfigMap.Name = OdfInfoConfigMapName
	odfInfoConfigMap.Namespace = operatorNamespace
	if err = r.Client.Get(r.ctx, client.ObjectKeyFromObject(odfInfoConfigMap), odfInfoConfigMap); err != nil {
		if errors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, err
	}

	mutex.Lock()
	defer mutex.Unlock()
	if len(odfInfoConfigMap.Data) > 1 {
		odfInfoKeyName := obj.getOdfInfoKeyName(storageCluster)
		delete(odfInfoConfigMap.Data, odfInfoKeyName)
		if err = r.Client.Update(r.ctx, odfInfoConfigMap); err != nil && !errors.IsNotFound(err) {
			r.Log.Error(err, "Failed to update odf-info config map with deleted key.", "ConfigMap", client.ObjectKeyFromObject(odfInfoConfigMap), "Key", odfInfoKeyName)
			return reconcile.Result{}, fmt.Errorf("failed to delete key %v in odf-info %v: %v", odfInfoKeyName, odfInfoConfigMap.Name, err)
		}
	} else {
		if err = r.Client.Delete(r.ctx, odfInfoConfigMap); err != nil && !errors.IsNotFound(err) {
			r.Log.Error(err, "Failed to delete odf-info config map.", "ConfigMap", client.ObjectKeyFromObject(odfInfoConfigMap))
			return reconcile.Result{}, fmt.Errorf("failed to delete odf-info %v: %v", odfInfoConfigMap.Name, err)
		}
	}
	return reconcile.Result{}, nil
}

func getOdfInfoData(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) (string, error) {
	ocsVersion, err := getOcsVersion(r)
	if err != nil {
		return "", err
	}
	cephFSId, err := getCephFsid(r, storageCluster)
	if err != nil {
		return "", err
	}

	odfDeploymentType := odfDeploymentTypeExternal
	if !storageCluster.Spec.ExternalStorage.Enable {
		odfDeploymentType = odfDeploymentTypeInternal
	}
	var storageSystemName string
	if storageSystemName, err = getStorageSystemName(storageCluster); err != nil {
		return "", err
	}

	connectedClients, err := getConnectedClients(r, storageCluster)
	if err != nil {
		return "", err
	}

	var isDROptimized = "false"
	// Set isDROptmized to "false" in case of external clusters as we currently don't have to way to determine
	// if external cluster OSDs are using bluestore-rdr
	if !storageCluster.Spec.ExternalStorage.Enable && !testSkipIsDrOptimized {
		isDROptimized, err = getIsDROptimized(r, storageCluster)
		if err != nil {
			r.Log.Error(err, "failed to get cephcluster status. retrying again")
			return "", err
		}
	}

	data := ocsv1a1.OdfInfoData{
		Version:           ocsVersion,
		DeploymentType:    odfDeploymentType,
		StorageSystemName: storageSystemName,
		Clients:           connectedClients,
		StorageCluster: ocsv1a1.InfoStorageCluster{
			NamespacedName:          client.ObjectKeyFromObject(storageCluster),
			StorageProviderEndpoint: storageCluster.Status.StorageProviderEndpoint,
			CephClusterFSID:         cephFSId,
			IsDrOptimized:           fmt.Sprintf("%v", isDROptimized),
		},
	}
	yamlData, err := yaml.Marshal(data)
	if err != nil {
		return "", err
	}
	return string(yamlData), nil

}

func getConnectedClients(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) ([]ocsv1a1.ConnectedClient, error) {
	storageConsumers := &ocsv1a1.StorageConsumerList{}
	err := r.Client.List(r.ctx, storageConsumers, client.InNamespace(storageCluster.Namespace))
	if err != nil {
		return nil, err
	}
	connectedClients := make([]ocsv1a1.ConnectedClient, 0, len(storageConsumers.Items))

	for storageConsumerIdx := range storageConsumers.Items {
		storageConsumer := &storageConsumers.Items[storageConsumerIdx]
		clusterID := storageConsumer.Status.Client.ClusterID
		name := storageConsumer.Status.Client.Name
		newConnectedClient := ocsv1a1.ConnectedClient{
			Name:      name,
			ClusterID: clusterID,
		}
		connectedClients = append(connectedClients, newConnectedClient)
	}

	return connectedClients, nil
}

func getStorageSystemName(storageCluster *ocsv1.StorageCluster) (string, error) {
	storageSystemRef := util.Find(storageCluster.OwnerReferences, func(ref *metav1.OwnerReference) bool {
		return ref.Kind == "StorageSystem"
	})
	if storageSystemRef != nil {
		return storageSystemRef.Name, nil
	}

	return "", fmt.Errorf(
		"failed to find parent StorageSystem's name in StorageCluster %q"+
			" ownerreferences, %v",
		storageCluster.Name,
		storageCluster.OwnerReferences)

}

func getOcsVersion(r *StorageClusterReconciler) (string, error) {
	var csvs operatorsv1alpha1.ClusterServiceVersionList
	err := r.Client.List(r.ctx, &csvs, client.InNamespace(r.OperatorNamespace))
	if err != nil {
		return "", err
	}

	csv := util.Find(csvs.Items, func(csv *operatorsv1alpha1.ClusterServiceVersion) bool {
		return strings.HasPrefix(csv.Name, ocsOperatorNamePrefix)
	})
	if csv == nil {
		return "", fmt.Errorf("failed to find csv with prefix %q", ocsOperatorNamePrefix)
	}
	return csv.Spec.Version.String(), nil
}

func getCephFsid(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) (string, error) {
	rookCephMonSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      rookCephMonSecretName,
			Namespace: storageCluster.Namespace,
		},
	}

	if err := r.Client.Get(r.ctx, client.ObjectKeyFromObject(rookCephMonSecret), rookCephMonSecret); err != nil {
		return "", err
	}
	var val []byte
	var ok bool
	if val, ok = rookCephMonSecret.Data[fsidKey]; !ok {
		return "", fmt.Errorf("failed to fetch ceph fsid from %q secret", rookCephMonSecretName)
	}

	return string(val), nil
}

func getIsDROptimized(r *StorageClusterReconciler, storageCluster *ocsv1.StorageCluster) (string, error) {
	var cephCluster rookCephv1.CephCluster
	err := r.Client.Get(r.ctx, types.NamespacedName{Name: generateNameForCephClusterFromString(storageCluster.Name), Namespace: storageCluster.Namespace}, &cephCluster)
	if err != nil {
		return "false", err
	}
	if cephCluster.Status.CephStorage == nil || cephCluster.Status.CephStorage.OSD.StoreType == nil {
		return "false", fmt.Errorf("cephcluster status does not have OSD store information")
	}
	bluestorerdr, ok := cephCluster.Status.CephStorage.OSD.StoreType["bluestore-rdr"]
	if !ok {
		return "false", nil
	}
	total := getOsdCount(storageCluster)
	if bluestorerdr < total {
		return "false", nil
	}
	return "true", nil
}
