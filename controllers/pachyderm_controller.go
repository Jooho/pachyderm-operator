/*
Copyright 2021 Pachyderm.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	aimlv1beta1 "github.com/opdev/pachyderm-operator/api/v1beta1"
	"github.com/opdev/pachyderm-operator/controllers/generators"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
)

const (
	pachydermFinalizer string = "finalizer.pachyderm.com"

	// ErrEtcdNotReady is returned when Etcd is not ready
	ErrEtcdNotReady generators.PachydermError = "waiting for etcd"
)

// PachydermReconciler reconciles a Pachyderm object
type PachydermReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=aiml.pachyderm.com,resources=pachyderms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=aiml.pachyderm.com,resources=pachyderms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=aiml.pachyderm.com,resources=pachyderms/finalizers,verbs=update
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;watch;create;update;patch;delete;deletecollection
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=storage.k8s.io,resources=storageclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=pods/log,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=endpoints,verbs=get;list;watch
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=replicationcontrollers/scale,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=security.openshift.io,resources=securitycontextconstraints,resourceNames=anyuid,verbs=use

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.7.2/pkg/reconcile
func (r *PachydermReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	_ = r.Log.WithValues("pachyderm", req.NamespacedName)

	pd := &aimlv1beta1.Pachyderm{}
	if err := r.Get(ctx, req.NamespacedName, pd); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcileFinalizer(ctx, pd); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileStatus(ctx, pd); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		if errors.IsResourceExpired(err) {
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, err
	}

	if err := r.reconcilePachydermObj(ctx, pd); err != nil {
		if err == ErrEtcdNotReady {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *PachydermReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&aimlv1beta1.Pachyderm{}).
		Owns(&networkingv1.Ingress{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.Secret{}).
		Owns(&rbacv1.Role{}).
		Owns(&rbacv1.RoleBinding{}).
		WithEventFilter(filterEvents()).
		Complete(r)
}

func filterEvents() predicate.Funcs {
	return predicate.Funcs{
		DeleteFunc: func(event.DeleteEvent) bool {
			// enable sending delete functions
			// to the reconcile function
			return true
		},
	}
}

func (r *PachydermReconciler) validatePachyderm(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()
	if pd.Spec.Pachd.Storage.Google != nil {
		credentials, err := r.googleCredentialsJSON(ctx, components.Parent())
		if err != nil {
			return err
		}

		components.SetGoogleCredentials(credentials)
	}

	return nil
}

func (r *PachydermReconciler) googleCredentialsJSON(ctx context.Context, pd *aimlv1beta1.Pachyderm) ([]byte, error) {
	gcsKey := types.NamespacedName{
		Namespace: pd.Namespace,
		Name:      pd.Spec.Pachd.Storage.Google.CredentialSecret,
	}
	credentialSecret := &corev1.Secret{}
	if err := r.Get(ctx, gcsKey, credentialSecret); err != nil {
		return []byte{}, err
	}

	if credentialsJSON, ok := credentialSecret.Data["credentials.json"]; ok {
		return credentialsJSON, nil
	}

	return []byte{}, nil
}

func (r *PachydermReconciler) reconcilePachydermObj(ctx context.Context, pd *aimlv1beta1.Pachyderm) error {
	components := generators.Prepare(pd)

	// perform pre-checks
	if err := r.validatePachyderm(ctx, components); err != nil {
		return err
	}

	// Deploy service accounts
	if err := r.reconcileServiceAccounts(ctx, components); err != nil {
		return err
	}

	// roles
	if err := r.reconcileRoles(ctx, components); err != nil {
		return err
	}

	// role bindings
	if err := r.reconcileRoleBindings(ctx, components); err != nil {
		return err
	}

	// cluster roles
	if err := r.reconcileClusterRoles(ctx, components); err != nil {
		return err
	}

	// cluster role bindings
	if err := r.reconcileClusterRoleBindings(ctx, components); err != nil {
		return err
	}

	// Deploy secrets
	if err := r.reconcileSecrets(ctx, components); err != nil {
		return err
	}

	// Deploy configmaps
	if err := r.reconcileConfigMaps(ctx, components); err != nil {
		return err
	}

	// Deploy services
	if err := r.reconcileServices(ctx, components); err != nil {
		return err
	}

	// Deploy storage class
	if err := r.reconcileStorageClass(ctx, components); err != nil {
		return err
	}

	if err := r.deployEtcd(ctx, components); err != nil {
		return err
	}

	if err := r.deployPostgres(ctx, components); err != nil {
		return err
	}

	if err := r.deployPachd(ctx, components); err != nil {
		return err
	}

	if err := r.deployDash(ctx, components); err != nil {
		return err
	}

	return nil
}

// TODO: cleanup Pachyderm objects
// - service accounts
func (r *PachydermReconciler) cleanupPachydermResources(ctx context.Context, pd *aimlv1beta1.Pachyderm) error {
	pds := &aimlv1beta1.PachydermList{}
	if err := r.List(ctx, pds, client.InNamespace(pd.Namespace)); err != nil {
		return err
	}

	// delete cluster resources
	components := generators.Prepare(pd)
	if len(pds.Items) <= 1 {
		// delete roles
		for _, role := range components.Roles {
			if err := r.Delete(ctx, &role); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}

		// delete role bindings
		for _, rb := range components.RoleBindings {
			if err := r.Delete(ctx, &rb); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}

		// delete service accounts
		for _, sa := range components.ServiceAccounts {
			if err := r.Delete(ctx, &sa); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}
	}

	// clean up cluster resources
	if err := r.List(ctx, pds); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	if len(pds.Items) <= 1 {
		// delete cluster role bindings
		for _, crb := range components.ClusterRoleBindings {
			if err := r.Delete(ctx, &crb); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}

		// delete cluster roles
		for _, cr := range components.ClusterRoles {
			if err := r.Delete(ctx, &cr); err != nil {
				if errors.IsNotFound(err) {
					return nil
				}
				return err
			}
		}
	}

	return nil
}

// TODO: set finalizer and status for Pachyderm resource
func (r *PachydermReconciler) reconcileStatus(ctx context.Context, pd *aimlv1beta1.Pachyderm) error {
	current := &aimlv1beta1.Pachyderm{}
	pdKey := types.NamespacedName{
		Name:      pd.Name,
		Namespace: pd.Namespace,
	}

	if err := r.Get(ctx, pdKey, current); err != nil {
		if errors.IsNotFound(err) {
			// TODO: do something
			return nil
		}
		return err
	}

	if pd.DeletionTimestamp != nil && pd.Status.Phase != aimlv1beta1.PhaseDeleting {
		current.Status.Phase = aimlv1beta1.PhaseDeleting
	}

	if reflect.DeepEqual(current.Status, aimlv1beta1.PachydermStatus{}) &&
		pd.DeletionTimestamp == nil {
		current.Status.Phase = aimlv1beta1.PhaseInitializing
	}

	if r.isPachydermRunning(ctx, pd) && pd.DeletionTimestamp == nil {
		current.Status.Phase = aimlv1beta1.PhaseRunning
	}

	if err := r.Status().Patch(ctx, current, client.MergeFrom(pd)); err != nil {
		if errors.IsResourceExpired(err) {
			return nil
		}
		return err
	}

	return nil
}

func (r *PachydermReconciler) isPachydermRunning(ctx context.Context, pd *aimlv1beta1.Pachyderm) bool {
	// check status of etcd
	etcdSvc := types.NamespacedName{
		Name:      "etcd",
		Namespace: pd.Namespace,
	}
	if !r.isServiceReady(ctx, etcdSvc) {
		return false
	}

	// check status of pachd
	pachdSvc := types.NamespacedName{
		Name:      "pachd",
		Namespace: pd.Namespace,
	}
	if !r.isServiceReady(ctx, pachdSvc) {
		return false
	}

	if !pd.Spec.Dashd.Disable {
		// check status of dash
		dashSvc := types.NamespacedName{
			Name:      "dash",
			Namespace: pd.Namespace,
		}
		if !r.isServiceReady(ctx, dashSvc) {
			return false
		}
	}

	// pachd-peer connection test
	return testPachdPeerConnection(ctx, pd)
}

func testPachdPeerConnection(ctx context.Context, pd *aimlv1beta1.Pachyderm) bool {
	hostname := strings.Join([]string{"pachd-peer", pd.Namespace}, ".")
	pachdPeer := fmt.Sprintf("%s:%s", hostname, "30653")

	conn, err := net.Dial("tcp", pachdPeer)
	if err != nil {
		return false
	}
	defer conn.Close()

	return (conn != nil)
}

func (r *PachydermReconciler) reconcileFinalizer(ctx context.Context, pd *aimlv1beta1.Pachyderm) error {
	currentFinalizers := pd.Finalizers

	// add finalizer for new Pachyderm resource
	if pd.DeletionTimestamp == nil && !controllerutil.ContainsFinalizer(pd, pachydermFinalizer) {
		controllerutil.AddFinalizer(pd, pachydermFinalizer)
	}

	// perform clean up and delete finalizer otherwise
	if pd.DeletionTimestamp != nil && controllerutil.ContainsFinalizer(pd, pachydermFinalizer) {
		if err := r.cleanupPachydermResources(ctx, pd); err != nil {
			return err
		}
		// remove finalizer if clean up is successful
		controllerutil.RemoveFinalizer(pd, pachydermFinalizer)
	}

	if reflect.DeepEqual(pd.Finalizers, currentFinalizers) {
		return nil
	}

	return r.Update(ctx, pd)
}

// TODO(OchiengEd): remove owner reference and use finalizers to clean up service accounts
func (r *PachydermReconciler) reconcileServiceAccounts(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	for _, sa := range components.ServiceAccounts {
		// add owner references
		if err := controllerutil.SetControllerReference(pd, &sa, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, &sa); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}
	}

	return nil
}

// TODO(OchiengEd): remove owner reference and use finalizers to clean up roles
func (r *PachydermReconciler) reconcileRoles(ctx context.Context, components *generators.PachydermComponents) error {

	for _, role := range components.Roles {
		// add owner references
		if err := controllerutil.SetControllerReference(components.Parent(), &role, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, &role); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) reconcileClusterRoles(ctx context.Context, components *generators.PachydermComponents) error {

	for _, clusterrole := range components.ClusterRoles {

		if err := r.Create(ctx, &clusterrole); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}
	}
	return nil
}

func (r *PachydermReconciler) reconcileRoleBindings(ctx context.Context, components *generators.PachydermComponents) error {

	for _, rolebinding := range components.RoleBindings {
		// add owner references
		if err := controllerutil.SetControllerReference(components.Parent(), &rolebinding, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, &rolebinding); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) reconcileClusterRoleBindings(ctx context.Context, components *generators.PachydermComponents) error {
	for _, crb := range components.ClusterRoleBindings {

		if err := r.Create(ctx, &crb); err != nil {
			if errors.IsAlreadyExists(err) {
				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) reconcileServices(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	for _, svc := range components.Services {
		// add owner references
		if err := controllerutil.SetControllerReference(pd, &svc, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, &svc); err != nil {
			if errors.IsAlreadyExists(err) {
				// Check if the secret contents have changed
				current := &corev1.Service{}
				svcKey := types.NamespacedName{
					Name:      svc.Name,
					Namespace: pd.Namespace,
				}

				if err := r.Get(ctx, svcKey, current); err != nil {
					return err
				}

				if serviceChanged(current, &svc) {
					if err := r.Update(ctx, current); err != nil {
						return err
					}
				}

				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) reconcileSecrets(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	for _, secret := range components.Secrets() {
		// set owner reference
		if err := controllerutil.SetControllerReference(pd, secret, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, secret); err != nil {
			if errors.IsAlreadyExists(err) {
				// Check if the secret contents have changed
				currentSecret := &corev1.Secret{}
				secretKey := types.NamespacedName{
					Name:      secret.Name,
					Namespace: pd.Namespace,
				}

				if err := r.Get(ctx, secretKey, currentSecret); err != nil {
					return err
				}

				if !reflect.DeepEqual(secret.Data, currentSecret.Data) {
					currentSecret.Data = secret.Data

					if err := r.Update(ctx, currentSecret); err != nil {
						return err
					}
				}
				// secret exists
				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) reconcileConfigMaps(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	for _, cm := range components.ConfgigMaps() {
		// set owner reference
		if err := controllerutil.SetControllerReference(pd, cm, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, cm); err != nil {
			if errors.IsAlreadyExists(err) {
				// Check if the secret contents have changed
				currentConfigMap := &corev1.ConfigMap{}
				cmKey := types.NamespacedName{
					Name:      cm.Name,
					Namespace: pd.Namespace,
				}

				if err := r.Get(ctx, cmKey, currentConfigMap); err != nil {
					return err
				}

				if !reflect.DeepEqual(cm.Data, currentConfigMap.Data) {
					currentConfigMap.Data = cm.Data

					if err := r.Update(ctx, currentConfigMap); err != nil {
						return err
					}
				}
				// configmap exists
				return nil
			}

			return err
		}
	}

	return nil
}

func (r *PachydermReconciler) deployEtcd(ctx context.Context, components *generators.PachydermComponents) error {
	etcd := components.EtcdStatefulSet()
	if err := controllerutil.SetControllerReference(components.Parent(), etcd, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, etcd); err != nil {
		if errors.IsAlreadyExists(err) {
			// TODO: add update logic
			return nil
		}
		return err
	}

	return nil
}

func (r *PachydermReconciler) deployPostgres(ctx context.Context, components *generators.PachydermComponents) error {
	postgres := components.PostgreStatefulset()
	if err := controllerutil.SetControllerReference(components.Parent(), postgres, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, postgres); err != nil {
		if errors.IsAlreadyExists(err) {
			// TODO: add update logic
			return nil
		}
		return err
	}

	return nil
}

func (r *PachydermReconciler) deployPachd(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	// Check Etcd is ready before deploying pachd
	etcdSvc := types.NamespacedName{
		Name:      "etcd",
		Namespace: pd.Namespace,
	}
	if !r.isServiceReady(ctx, etcdSvc) {
		return ErrEtcdNotReady
	}

	pachd := components.PachdDeployment()
	if err := controllerutil.SetControllerReference(pd, pachd, r.Scheme); err != nil {
		return err
	}

	if err := r.Create(ctx, pachd); err != nil {
		if errors.IsAlreadyExists(err) {
			// TODO: add update logic
			return nil
		}
		return err
	}

	return nil
}

func (r *PachydermReconciler) deployDash(ctx context.Context, components *generators.PachydermComponents) error {
	pd := components.Parent()

	if !pd.Spec.Dashd.Disable {
		dash := components.DashDeployment()
		if err := controllerutil.SetControllerReference(pd, dash, r.Scheme); err != nil {
			return err
		}

		if err := r.Create(ctx, dash); err != nil {
			if errors.IsAlreadyExists(err) {
				// TODO: add update logic
				return nil
			}
			return err
		}
	}
	return nil
}

func (r *PachydermReconciler) reconcileStorageClass(ctx context.Context, components *generators.PachydermComponents) error {
	pachyderm := components.Parent()
	storageClassName := generators.EtcdStorageClassName(pachyderm)
	if storageClassName != "etcd-storage-class" {
		userStorageClass := &storagev1.StorageClass{}
		userSCKey := types.NamespacedName{
			Name: storageClassName,
		}
		if err := r.Get(ctx, userSCKey, userStorageClass); err != nil {
			return err
		}
		return nil
	}

	sc := components.StorageClass()
	if err := r.Create(ctx, sc); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
	}

	return nil
}

func (r *PachydermReconciler) isServiceReady(ctx context.Context, service types.NamespacedName) bool {
	ep := &corev1.Endpoints{}
	if err := r.Get(ctx, service, ep); err != nil {
		return false
	}

	addresses := []corev1.EndpointAddress{}

	for _, subset := range ep.Subsets {
		addresses = append(addresses, subset.Addresses...)
	}

	return len(addresses) > 0
}
