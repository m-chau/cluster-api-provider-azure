/*
Copyright 2024 The Kubernetes Authors.

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
	"errors"
	"fmt"

	asocontainerservicev1 "github.com/Azure/azure-service-operator/v2/api/containerservice/v1api20231001"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	infracontroller "sigs.k8s.io/cluster-api-provider-azure/controllers"
	infrav1exp "sigs.k8s.io/cluster-api-provider-azure/exp/api/v1alpha1"
	"sigs.k8s.io/cluster-api-provider-azure/util/tele"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/controllers/external"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var errInvalidClusterKind = errors.New("AzureASOManagedControlPlane cannot be used without AzureASOManagedCluster")

// AzureASOManagedControlPlaneReconciler reconciles a AzureASOManagedControlPlane object.
type AzureASOManagedControlPlaneReconciler struct {
	client.Client
	WatchFilterValue string

	newResourceReconciler func(*infrav1exp.AzureASOManagedControlPlane, []*unstructured.Unstructured) resourceReconciler
}

// SetupWithManager sets up the controller with the Manager.
func (r *AzureASOManagedControlPlaneReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	_, log, done := tele.StartSpanWithLogger(ctx,
		"controllers.AzureASOManagedControlPlaneReconciler.SetupWithManager",
		tele.KVP("controller", infrav1exp.AzureASOManagedControlPlaneKind),
	)
	defer done()

	c, err := ctrl.NewControllerManagedBy(mgr).
		For(&infrav1exp.AzureASOManagedControlPlane{}).
		WithEventFilter(predicates.ResourceHasFilterLabel(log, r.WatchFilterValue)).
		Watches(&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(clusterToAzureASOManagedControlPlane),
			builder.WithPredicates(
				predicates.ResourceHasFilterLabel(log, r.WatchFilterValue),
				infracontroller.ClusterPauseChangeAndInfrastructureReady(log),
			),
		).
		Build(r)
	if err != nil {
		return err
	}

	externalTracker := &external.ObjectTracker{
		Cache:      mgr.GetCache(),
		Controller: c,
	}

	r.newResourceReconciler = func(asoManagedCluster *infrav1exp.AzureASOManagedControlPlane, resources []*unstructured.Unstructured) resourceReconciler {
		return &ResourceReconciler{
			Client:    r.Client,
			resources: resources,
			owner:     asoManagedCluster,
			watcher:   externalTracker,
		}
	}

	return nil
}

func clusterToAzureASOManagedControlPlane(_ context.Context, o client.Object) []ctrl.Request {
	controlPlaneRef := o.(*clusterv1.Cluster).Spec.ControlPlaneRef
	if controlPlaneRef != nil &&
		controlPlaneRef.APIVersion == infrav1exp.GroupVersion.Identifier() &&
		controlPlaneRef.Kind == infrav1exp.AzureASOManagedControlPlaneKind {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: controlPlaneRef.Namespace, Name: controlPlaneRef.Name}}}
	}
	return nil
}

//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=azureasomanagedcontrolplanes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=azureasomanagedcontrolplanes/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=azureasomanagedcontrolplanes/finalizers,verbs=update

// Reconcile reconciles an AzureASOManagedControlPlane.
func (r *AzureASOManagedControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, resultErr error) {
	ctx, _, done := tele.StartSpanWithLogger(ctx,
		"controllers.AzureASOManagedControlPlaneReconciler.Reconcile",
		tele.KVP("namespace", req.Namespace),
		tele.KVP("name", req.Name),
		tele.KVP("kind", infrav1exp.AzureASOManagedControlPlaneKind),
	)
	defer done()

	asoManagedControlPlane := &infrav1exp.AzureASOManagedControlPlane{}
	err := r.Get(ctx, req.NamespacedName, asoManagedControlPlane)
	if err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	patchHelper, err := patch.NewHelper(asoManagedControlPlane, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to create patch helper: %w", err)
	}
	defer func() {
		err := patchHelper.Patch(ctx, asoManagedControlPlane)
		if err != nil && resultErr == nil {
			resultErr = err
			result = ctrl.Result{}
		}
	}()

	cluster, err := util.GetOwnerCluster(ctx, r.Client, asoManagedControlPlane.ObjectMeta)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster != nil && cluster.Spec.Paused ||
		annotations.HasPaused(asoManagedControlPlane) {
		return r.reconcilePaused(ctx, asoManagedControlPlane, cluster)
	}

	if !asoManagedControlPlane.GetDeletionTimestamp().IsZero() {
		return r.reconcileDelete(ctx, asoManagedControlPlane)
	}

	return r.reconcileNormal(ctx, asoManagedControlPlane, cluster)
}

func (r *AzureASOManagedControlPlaneReconciler) reconcileNormal(ctx context.Context, asoManagedControlPlane *infrav1exp.AzureASOManagedControlPlane, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	ctx, log, done := tele.StartSpanWithLogger(ctx,
		"controllers.AzureASOManagedControlPlaneReconciler.reconcileNormal",
	)
	defer done()
	log.V(4).Info("reconciling normally")

	if cluster == nil {
		log.V(4).Info("Cluster Controller has not yet set OwnerRef")
		return ctrl.Result{}, nil
	}
	if cluster.Spec.InfrastructureRef == nil ||
		cluster.Spec.InfrastructureRef.APIVersion != infrav1exp.GroupVersion.Identifier() ||
		cluster.Spec.InfrastructureRef.Kind != infrav1exp.AzureASOManagedClusterKind {
		return ctrl.Result{}, reconcile.TerminalError(errInvalidClusterKind)
	}

	needsPatch := controllerutil.AddFinalizer(asoManagedControlPlane, infrav1exp.AzureASOManagedControlPlaneFinalizer)
	if needsPatch {
		return ctrl.Result{Requeue: true}, nil
	}

	us, err := resourcesToUnstructured(asoManagedControlPlane.Spec.Resources)
	if err != nil {
		return ctrl.Result{}, err
	}

	var managedClusterName string
	for _, resource := range us {
		if resource.GroupVersionKind().Group == asocontainerservicev1.GroupVersion.Group &&
			resource.GroupVersionKind().Kind == "ManagedCluster" {
			managedClusterName = resource.GetName()
			break
		}
	}
	if managedClusterName == "" {
		return ctrl.Result{}, reconcile.TerminalError(fmt.Errorf("no %s ManagedCluster defined in AzureASOManagedControlPlane spec.resources", asocontainerservicev1.GroupVersion.Group))
	}

	resourceReconciler := r.newResourceReconciler(asoManagedControlPlane, us)
	err = resourceReconciler.Reconcile(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile resources: %w", err)
	}
	for _, status := range asoManagedControlPlane.Status.Resources {
		if !status.Ready {
			return ctrl.Result{}, nil
		}
	}

	managedCluster := &asocontainerservicev1.ManagedCluster{}
	err = r.Get(ctx, client.ObjectKey{Namespace: asoManagedControlPlane.Namespace, Name: managedClusterName}, managedCluster)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("error getting ManagedCluster: %w", err)
	}

	asoManagedControlPlane.Status.ControlPlaneEndpoint = getControlPlaneEndpoint(managedCluster)

	return ctrl.Result{}, nil
}

//nolint:unparam // these parameters will be used soon enough
func (r *AzureASOManagedControlPlaneReconciler) reconcilePaused(ctx context.Context, asoManagedControlPlane *infrav1exp.AzureASOManagedControlPlane, cluster *clusterv1.Cluster) (ctrl.Result, error) {
	//nolint:all // ctx will be used soon
	ctx, log, done := tele.StartSpanWithLogger(ctx,
		"controllers.AzureASOManagedControlPlaneReconciler.reconcilePaused",
	)
	defer done()
	log.V(4).Info("reconciling pause")

	return ctrl.Result{}, nil
}

//nolint:unparam // an empty ctrl.Result is always returned here, leaving it as-is to avoid churn in refactoring later if that changes.
func (r *AzureASOManagedControlPlaneReconciler) reconcileDelete(ctx context.Context, asoManagedControlPlane *infrav1exp.AzureASOManagedControlPlane) (ctrl.Result, error) {
	ctx, log, done := tele.StartSpanWithLogger(ctx,
		"controllers.AzureASOManagedControlPlaneReconciler.reconcileDelete",
	)
	defer done()
	log.V(4).Info("reconciling delete")

	us, err := resourcesToUnstructured(asoManagedControlPlane.Spec.Resources)
	if err != nil {
		return ctrl.Result{}, err
	}
	resourceReconciler := r.newResourceReconciler(asoManagedControlPlane, us)
	err = resourceReconciler.Delete(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("failed to reconcile resources: %w", err)
	}
	if len(asoManagedControlPlane.Status.Resources) > 0 {
		return ctrl.Result{}, nil
	}

	controllerutil.RemoveFinalizer(asoManagedControlPlane, infrav1exp.AzureASOManagedControlPlaneFinalizer)
	return ctrl.Result{}, nil
}

func getControlPlaneEndpoint(managedCluster *asocontainerservicev1.ManagedCluster) clusterv1.APIEndpoint {
	if managedCluster.Status.PrivateFQDN != nil {
		return clusterv1.APIEndpoint{
			Host: *managedCluster.Status.PrivateFQDN,
			Port: 443,
		}
	}
	if managedCluster.Status.Fqdn != nil {
		return clusterv1.APIEndpoint{
			Host: *managedCluster.Status.Fqdn,
			Port: 443,
		}
	}
	return clusterv1.APIEndpoint{}
}
