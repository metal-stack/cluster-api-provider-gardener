/*
Copyright 2024.

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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	authenticationv1alpha1 "github.com/gardener/gardener/pkg/apis/authentication/v1alpha1"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"github.com/go-logr/logr"
	"github.com/metal-stack/cluster-api-provider-gardener/api/v1alpha1"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/conditions"
	"sigs.k8s.io/cluster-api/util/patch"
	capisecret "sigs.k8s.io/cluster-api/util/secret"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/conversion"
)

// GardenerClusterReconciler reconciles a GardenerCluster object
type GardenerClusterReconciler struct {
	GardenerClient client.Client
	client.Client
	Scheme *runtime.Scheme
}

type clusterReconciler struct {
	gardenerClient client.Client
	client         client.Client
	ctx            context.Context
	log            logr.Logger
	cluster        *clusterv1.Cluster
	infraCluster   *v1alpha1.GardenerCluster
	decoder        *conversion.Decoder
	shootSpec      gardencorev1beta1.ShootSpec
}

// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=gardenerclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=gardenerclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=gardenerclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the GardenerCluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.19.0/pkg/reconcile
func (r *GardenerClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var (
		log          = ctrllog.FromContext(ctx)
		infraCluster = &v1alpha1.GardenerCluster{}
	)

	if err := r.Client.Get(ctx, req.NamespacedName, infraCluster); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("resource no longer exists")
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, err
	}

	cluster, err := util.GetOwnerCluster(ctx, r.Client, infraCluster.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster == nil {
		log.Info("infrastructure cluster resource has no ownership yet")
		return ctrl.Result{}, nil
	}

	reconciler := &clusterReconciler{
		gardenerClient: r.GardenerClient,
		client:         r.Client,
		ctx:            ctx,
		log:            log,
		cluster:        cluster,
		infraCluster:   infraCluster,
		decoder:        conversion.NewDecoder(r.GardenerClient.Scheme()),
	}

	defer func() {
		statusErr := reconciler.status()
		if statusErr != nil {
			err = errors.Join(err, fmt.Errorf("unable to update status: %w", statusErr))
		} else if !reconciler.infraCluster.Status.Ready {
			err = errors.New("cluster is not yet ready, requeueing")
		}
	}()

	if !infraCluster.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(infraCluster, v1alpha1.ClusterFinalizer) {
			return ctrl.Result{}, nil
		}

		log.Info("reconciling resource deletion flow")
		err := reconciler.delete()
		if err != nil {
			return ctrl.Result{}, err
		}

		log.Info("deletion finished, removing finalizer")
		controllerutil.RemoveFinalizer(infraCluster, v1alpha1.ClusterFinalizer)
		if err := r.Client.Update(ctx, infraCluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to remove finalizer: %w", err)
		}

		return ctrl.Result{}, nil
	}

	log.Info("reconciling cluster")

	if !controllerutil.ContainsFinalizer(infraCluster, v1alpha1.ClusterFinalizer) {
		log.Info("adding finalizer")
		controllerutil.AddFinalizer(infraCluster, v1alpha1.ClusterFinalizer)
		if err := r.Client.Update(ctx, infraCluster); err != nil {
			return ctrl.Result{}, fmt.Errorf("unable to add finalizer: %w", err)
		}

		return ctrl.Result{}, nil
	}

	err = reconciler.reconcile()

	return ctrl.Result{}, err // remember to return err here and not nil because the defer func can influence this
}

// SetupWithManager sets up the controller with the Manager.
func (r *GardenerClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.GardenerCluster{}).
		Named("gardenercluster").
		Complete(r)
}

func (r *clusterReconciler) reconcile() error {
	var rawShoot gardencorev1beta1.Shoot
	err := r.decoder.DecodeInto(r.infraCluster.Spec.Shoot.Raw, &rawShoot)
	if err != nil {
		return fmt.Errorf("unable to decode shoot: %w", err)
	}

	r.shootSpec = rawShoot.Spec

	projectNamespace, err := r.ensureGardenProject()
	if err != nil {
		return fmt.Errorf("unable to ensure garden project: %w", err)
	}

	r.log.Info("ensured garden project", "project-namespace", projectNamespace)

	err = r.ensureSecretBinding(projectNamespace)
	if err != nil {
		return fmt.Errorf("unable to ensure secret binding: %w", err)
	}

	shoot := &gardencorev1beta1.Shoot{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.infraCluster.Name,
			Namespace: projectNamespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(r.ctx, r.gardenerClient, shoot, func() error {
		seedName := shoot.Spec.SeedName
		shoot.Spec = r.shootSpec
		shoot.Spec.SeedName = seedName

		shoot.Spec.SecretBindingName = ptr.To("provider-secret") // not sure if it's smart but this controller manages the secret, so...
		if shoot.Labels == nil {
			shoot.Labels = map[string]string{}
		}

		shoot.Labels[v1alpha1.TagInfraClusterID] = string(r.infraCluster.GetUID())

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create/update shoot: %w", err)
	}

	r.log.Info("ensured shoot", "name", shoot.Name, "namespace", shoot.Namespace)

	idx := slices.IndexFunc(shoot.Status.AdvertisedAddresses, func(address gardencorev1beta1.ShootAdvertisedAddress) bool {
		return address.Name == "external"
	})
	if idx >= 0 {
		r.log.Info("setting control plane endpoint into cluster resource")

		helper, err := patch.NewHelper(r.infraCluster, r.client)
		if err != nil {
			return err
		}

		r.infraCluster.Spec.ControlPlaneEndpoint = v1alpha1.APIEndpoint{
			Host: shoot.Status.AdvertisedAddresses[idx].URL,
			Port: 443,
		}

		err = helper.Patch(r.ctx, r.infraCluster) // TODO:check whether patch is not executed when no changes occur
		if err != nil {
			return fmt.Errorf("failed to update infra cluster control plane endpoint: %w", err)
		}
	}

	if err := r.kubeconfig(shoot); err != nil {
		return fmt.Errorf("error reconciling kubeconfig: %w", err)
	}

	r.log.Info("ensured kubeconfig")

	return err
}

func (r *clusterReconciler) ensureGardenProject() (string, error) {
	p, err := r.findGardenerProject()
	switch {
	case errors.Is(err, errGardenerObjectNotFound):
		// create happens after switch
		break
	case err != nil:
		return "", err
	default:
		if p.Spec.Namespace == nil {
			return "", fmt.Errorf("gardener project has no namespace set")
		}

		return *p.Spec.Namespace, nil
	}

	owner := "cluser-api-provider-gardener"

	p = &gardencorev1beta1.Project{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "capi-",
			Labels: map[string]string{
				v1alpha1.TagInfraClusterID: string(r.infraCluster.GetUID()),
			},
		},
		Spec: gardencorev1beta1.ProjectSpec{
			CreatedBy: &rbacv1.Subject{
				Kind:     rbacv1.UserKind,
				Name:     owner,
				APIGroup: "rbac.authorization.k8s.io",
			},
			Description: ptr.To("created by cluster-api-provider-gardener"),
			Owner: &rbacv1.Subject{
				Kind:     rbacv1.UserKind,
				Name:     owner,
				APIGroup: "rbac.authorization.k8s.io",
			},
			Members: []gardencorev1beta1.ProjectMember{
				{
					Subject: rbacv1.Subject{
						Kind:     rbacv1.UserKind,
						Name:     owner,
						APIGroup: "rbac.authorization.k8s.io",
					},
					Role: gardencorev1beta1.ProjectMemberAdmin,
				},
			},
			Purpose:     ptr.To(string(gardencorev1beta1.ShootPurposeProduction)),
			Tolerations: &gardencorev1beta1.ProjectTolerations{},
		},
	}

	err = r.gardenerClient.Create(r.ctx, p)
	if err != nil {
		return "", fmt.Errorf("error creating project: %w", err)
	}

	if p.Spec.Namespace == nil {
		return "", fmt.Errorf("gardener project has no namespace set")
	}

	return *p.Spec.Namespace, nil
}

var (
	errGardenerObjectNotFound      = errors.New("gardener object not found")
	errGardenerObjectMultipleFound = errors.New("more than one gardener object found in backend")
)

func (r *clusterReconciler) findGardenerProject() (*gardencorev1beta1.Project, error) {
	ps := &gardencorev1beta1.ProjectList{}
	err := r.gardenerClient.List(r.ctx, ps, client.MatchingLabels{v1alpha1.TagInfraClusterID: string(r.infraCluster.GetUID())})
	if err != nil {
		return nil, err
	}

	switch len(ps.Items) {
	case 0:
		return nil, errGardenerObjectNotFound
	case 1:
		return &ps.Items[0], nil
	default:
		return nil, errGardenerObjectMultipleFound
	}
}

func (r *clusterReconciler) findShoot() (*gardencorev1beta1.Shoot, error) {
	shoots := &gardencorev1beta1.ShootList{}
	err := r.gardenerClient.List(r.ctx, shoots, client.MatchingLabels{v1alpha1.TagInfraClusterID: string(r.infraCluster.GetUID())})
	if err != nil {
		return nil, err
	}

	switch len(shoots.Items) {
	case 0:
		return nil, errGardenerObjectNotFound
	case 1:
		return &shoots.Items[0], nil
	default:
		return nil, errGardenerObjectMultipleFound
	}
}

func (r *clusterReconciler) ensureSecretBinding(projectNamespace string) error {
	if r.shootSpec.CloudProfile == nil && r.shootSpec.CloudProfileName == nil {
		return errors.New("shoot spec must contain a cloud profile name")
	}

	providerSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      r.infraCluster.Spec.ProviderSecretRef.Name,
			Namespace: r.infraCluster.Spec.ProviderSecretRef.Namespace,
		},
	}
	err := r.client.Get(r.ctx, client.ObjectKeyFromObject(providerSecret), providerSecret)
	if err != nil {
		return fmt.Errorf("references provider secret not found: %w", err)
	}

	gardenerProviderSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider-secret",
			Namespace: projectNamespace,
		},
	}
	_, err = controllerutil.CreateOrUpdate(r.ctx, r.gardenerClient, gardenerProviderSecret, func() error {
		gardenerProviderSecret.Data = providerSecret.Data
		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create/update provider secret: %w", err)
	}

	sb := &gardencorev1beta1.SecretBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "provider-secret",
			Namespace: projectNamespace,
			Labels: map[string]string{
				"cloudprofile.garden.sapcloud.io/name": ptr.Deref(r.shootSpec.CloudProfileName, r.shootSpec.CloudProfile.Name),
			},
		},
		Provider: &gardencorev1beta1.SecretBindingProvider{
			Type: r.shootSpec.Provider.Type,
		},
	}

	_, err = controllerutil.CreateOrUpdate(r.ctx, r.gardenerClient, sb, func() error {
		sb.SecretRef = corev1.SecretReference{
			Name:      "provider-secret",
			Namespace: projectNamespace,
		}
		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

func (r *clusterReconciler) kubeconfig(shoot *gardencorev1beta1.Shoot) error {
	// TODO: do not rotate it every time...

	expiration := 8 * time.Hour
	expirationSeconds := int64(expiration.Seconds())

	adminKubeconfigRequest := &authenticationv1alpha1.AdminKubeconfigRequest{
		Spec: authenticationv1alpha1.AdminKubeconfigRequestSpec{
			ExpirationSeconds: &expirationSeconds,
		},
	}
	err := r.gardenerClient.SubResource("adminkubeconfig").Create(r.ctx, shoot, adminKubeconfigRequest)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      capisecret.Name(r.cluster.Name, capisecret.Kubeconfig),
			Namespace: r.infraCluster.Namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel: r.cluster.Name,
			},
		},
	}

	_, err = controllerutil.CreateOrUpdate(r.ctx, r.client, secret, func() error {
		secret.StringData = map[string]string{
			capisecret.KubeconfigDataName: string(adminKubeconfigRequest.Status.Kubeconfig),
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("unable to create/update kubeconfig secret: %w", err)
	}

	return nil
}

func (r *clusterReconciler) delete() error {
	var err error
	defer func() {
		statusErr := r.status()
		if statusErr != nil {
			err = errors.Join(err, fmt.Errorf("unable to update status: %w", statusErr))
		}
	}()

	shoot, err := r.findShoot()
	switch {
	case errors.Is(err, errGardenerObjectNotFound):
		// already deleted
		err = nil
	case err != nil:
		return err
	default:
		if shoot.Annotations == nil {
			shoot.Annotations = map[string]string{}
		}
		shoot.Annotations["confirmation.gardener.cloud/deletion"] = "true"

		err = r.gardenerClient.Update(r.ctx, shoot)
		if err != nil {
			return fmt.Errorf("unable to set confirmation deletion annotation: %w", err)
		}

		err = r.gardenerClient.Delete(r.ctx, shoot)
		if err != nil {
			return fmt.Errorf("error deleting shoot: %w", err)
		}

		return errors.New("shoot is being deleted")
	}

	r.log.Info("deleted shoot")

	p, err := r.findGardenerProject()
	switch {
	case errors.Is(err, errGardenerObjectNotFound):
		// already deleted
		err = nil
	case err != nil:
		return err
	default:
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations["confirmation.gardener.cloud/deletion"] = "true"

		err = r.gardenerClient.Update(r.ctx, p)
		if err != nil {
			return fmt.Errorf("unable to set confirmation deletion annotation: %w", err)
		}

		err = r.gardenerClient.Delete(r.ctx, p)
		if err != nil {
			return fmt.Errorf("unable to delete gardener project: %w", err)
		}

		return errors.New("gardener project is being deleted")
	}

	r.log.Info("deleted gardener project")

	return err
}

func (r *clusterReconciler) status() error {
	var (
		g, _             = errgroup.WithContext(r.ctx)
		conditionUpdates = make(chan func())

		// TODO: probably there is a helper for this available somewhere?
		allConditionsTrue = func() bool {
			for _, c := range r.infraCluster.Status.Conditions {
				if c.Status != corev1.ConditionTrue {
					return false
				}
			}

			return true
		}
	)

	defer func() {
		close(conditionUpdates)
	}()

	g.Go(func() error {
		shoot, err := r.findShoot()

		conditionUpdates <- func() {
			switch {
			case err != nil && !errors.Is(err, errGardenerObjectNotFound):
				conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerShootReady, "InternalError", clusterv1.ConditionSeverityError, "%s", err.Error())
			case errors.Is(err, errGardenerObjectNotFound):
				conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerShootReady, "NotCreated", clusterv1.ConditionSeverityError, "gardener shoot was not yet created")
			default:
				if shoot.DeletionTimestamp != nil && !shoot.DeletionTimestamp.IsZero() {
					conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerShootReady, "InDeletion", clusterv1.ConditionSeverityWarning, "gardener shoot is being deleted")
					break
				}

				anyConditionFailed := false
				for _, condition := range shoot.Status.Conditions {
					if condition.Status != gardencorev1beta1.ConditionTrue {
						conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerShootReady, "NotReady", clusterv1.ConditionSeverityWarning, fmt.Sprintf("gardener shoot is not yet ready, waiting for condition: %s", condition.Type))
						anyConditionFailed = true
						break
					}
				}

				if anyConditionFailed {
					return
				}

				conditions.MarkTrue(r.infraCluster, v1alpha1.GardenerShootReady)
			}
		}

		return err
	})

	g.Go(func() error {
		_, err := r.findGardenerProject()

		conditionUpdates <- func() {
			switch {
			case err != nil && !errors.Is(err, errGardenerObjectNotFound):
				conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerProjectEnsured, "InternalError", clusterv1.ConditionSeverityError, "%s", err.Error())
			case errors.Is(err, errGardenerObjectNotFound):
				conditions.MarkFalse(r.infraCluster, v1alpha1.GardenerProjectEnsured, "NotCreated", clusterv1.ConditionSeverityError, "gardener shoot was not yet created")
			default:
				conditions.MarkTrue(r.infraCluster, v1alpha1.GardenerProjectEnsured)
			}
		}

		return err
	})

	go func() {
		for u := range conditionUpdates {
			u()
		}
	}()

	groupErr := g.Wait()

	r.infraCluster.Status.Ready = groupErr == nil && allConditionsTrue()
	r.infraCluster.Status.Initialized = groupErr == nil && allConditionsTrue()

	err := r.client.Status().Update(r.ctx, r.infraCluster)

	return errors.Join(groupErr, err)
}
