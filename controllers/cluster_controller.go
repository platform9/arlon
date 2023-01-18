/*
Copyright 2021.

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
	"github.com/argoproj/argo-cd/v2/pkg/apiclient"
	argoapp "github.com/argoproj/argo-cd/v2/pkg/apiclient/application"
	"github.com/argoproj/argo-cd/v2/util/io"
	arlonv1 "github.com/arlonproj/arlon/api/v1"
	corev1 "github.com/arlonproj/arlon/api/v1"
	"github.com/arlonproj/arlon/pkg/argocd"
	bcl "github.com/arlonproj/arlon/pkg/basecluster"
	"github.com/arlonproj/arlon/pkg/cluster"
	"github.com/go-logr/logr"
	grpccodes "google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	restclient "k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"time"
)

var retryDelayAsResult = ctrl.Result{RequeueAfter: time.Second * 10}

// Default git location of Helm chart for Arlon app (for a cluster)
var defaultArlonChart = arlonv1.RepoSpec{
	Url:      "https://github.com/arlonproj/arlon.git",
	Path:     "pkg/cluster/manifests",
	Revision: "v0.10.0",
}

// ClusterReconciler reconciles a Cluster object
type ClusterReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	ArgocdClient apiclient.Client
	Config       *restclient.Config
	ArgoCdNs     string
	ArlonNs      string
}

//+kubebuilder:rbac:groups=core.arlon.io,resources=clusters,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core.arlon.io,resources=clusters/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=core.arlon.io,resources=clusters/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Cluster object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.13.0/pkg/reconcile
func (r *ClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)
	log.V(1).Info("arlon Cluster")
	var cr arlonv1.Cluster

	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("cluster is gone -- ok")
			return ctrl.Result{}, nil
		}
		log.Info(fmt.Sprintf("unable to get cluster (%s) ... requeuing", err))
		return ctrl.Result{Requeue: true}, nil
	}
	// Initialize the patch helper. It stores a "before" copy of the current object.
	patchHelper, err := patch.NewHelper(&cr, r.Client)
	if err != nil {
		log.Error(err, "Failed to configure the patch helper")
		return ctrl.Result{Requeue: true}, nil
	}
	if !cr.ObjectMeta.DeletionTimestamp.IsZero() {
		// Handle deletion reconciliation loop.
		return r.ReconcileDelete(ctx, log, &cr, patchHelper)
	}
	if cr.Status.State == "created" {
		log.V(1).Info("Cluster is already created")
		return ctrl.Result{}, nil
	}
	// Add finalizer first if not exist to avoid the race condition between init and delete
	if !controllerutil.ContainsFinalizer(&cr, arlonv1.ClusterFinalizer) {
		controllerutil.AddFinalizer(&cr, arlonv1.ClusterFinalizer)
		// patch and return right away instead of reusing the main defer,
		// because the main defer may take too much time to get cluster status
		// Patch ObservedGeneration only if the reconciliation completed successfully
		patchOpts := []patch.Option{patch.WithStatusObservedGeneration{}}
		if err := patchHelper.Patch(ctx, &cr, patchOpts...); err != nil {
			log.Error(err, "Failed to patch cluster to add finalizer")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	ctmpl := &cr.Spec.ClusterTemplate
	if cr.Status.InnerClusterName == "" {
		log.Info("validating cluster template ...")
		_, creds, err := argocd.GetKubeclientAndRepoCreds(r.Config, r.ArgoCdNs,
			ctmpl.Url)
		if err != nil {
			msg := fmt.Sprintf("failed to get repo creds: %s", err)
			return r.UpdateState(log, &cr, "retrying", msg, retryDelayAsResult)
		}
		innerClusterName, err := bcl.ValidateGitDir(creds, ctmpl.Url, ctmpl.Revision, ctmpl.Path)
		if err != nil {
			msg := fmt.Sprintf("failed to validate cluster template: %s", err)
			return r.UpdateState(log, &cr, "retrying", msg, retryDelayAsResult)
		}
		cr.Status.InnerClusterName = innerClusterName
		return r.UpdateState(log, &cr, "initializing",
			"cluster template validation successful", ctrl.Result{})
	}
	conn, appIf, err := r.ArgocdClient.NewApplicationClient()
	if err != nil {
		msg := fmt.Sprintf("failed to get argocd application client: %s", err)
		return r.UpdateState(log, &cr, "retrying", msg, retryDelayAsResult)
	}
	defer io.Close(conn)

	// Check if arlon app already exists
	arlonAppName := fmt.Sprintf("%s-arlon", cr.Name)
	_, err = appIf.Get(context.Background(), &argoapp.ApplicationQuery{Name: &arlonAppName})
	if err != nil {
		grpcStatus, ok := grpcstatus.FromError(err)
		if !ok {
			return r.UpdateState(log, &cr, "retrying",
				"failed to get grpc status from argocd API", retryDelayAsResult)
		}
		if grpcStatus.Code() != grpccodes.NotFound {
			return r.UpdateState(log, &cr, "retrying",
				fmt.Sprintf("unexpected grpc status: %d", grpcStatus.Code()),
				retryDelayAsResult)
		}
		helmChartInfo := cr.Spec.ArlonHelmChart
		if helmChartInfo == nil {
			helmChartInfo = &defaultArlonChart
		}
		casMgmtClusterHost := ""
		innerClusterName := ""
		gen2CASEnabled := cr.Spec.Autoscaler != nil
		if gen2CASEnabled {
			casMgmtClusterHost = cr.Spec.Autoscaler.MgmtClusterHost
			innerClusterName = cr.Status.InnerClusterName
		}
		arlonHelmChart := cr.Spec.ArlonHelmChart
		if arlonHelmChart == nil {
			arlonHelmChart = &defaultArlonChart
		}
		_, err = cluster.Create(appIf, r.Config, r.ArgoCdNs, r.ArlonNs,
			cr.Name, innerClusterName, arlonHelmChart.Url, arlonHelmChart.Revision,
			arlonHelmChart.Path, "",
			nil, false, casMgmtClusterHost, gen2CASEnabled)
		if err != nil {
			msg := fmt.Sprintf("failed to create arlon application: %s", err)
			return r.UpdateState(log, &cr, "retrying", msg, retryDelayAsResult)
		}
	}
	// Check if cluster app already exists
	_, err = appIf.Get(context.Background(), &argoapp.ApplicationQuery{Name: &cr.Name})
	if err == nil {
		// We're done
		return r.UpdateState(log, &cr, "created",
			"cluster app already exists -- ok", ctrl.Result{})
	}
	overridden := false
	_, err = cluster.CreateClusterApp(appIf, r.ArgoCdNs,
		cr.Name, cr.Status.InnerClusterName, ctmpl.Url, ctmpl.Revision,
		ctmpl.Path, false, overridden)
	if err != nil {
		msg := fmt.Sprintf("failed to create cluster application: %s", err)
		return r.UpdateState(log, &cr, "retrying", msg, retryDelayAsResult)
	}
	return r.UpdateState(log, &cr, "created",
		"cluster creation successful", retryDelayAsResult)
}

func (r *ClusterReconciler) UpdateState(
	log logr.Logger,
	cr *arlonv1.Cluster,
	state string,
	msg string,
	result ctrl.Result,
) (ctrl.Result, error) {
	cr.Status.State = state
	cr.Status.Message = msg
	log.Info(fmt.Sprintf("%s ... setting state to '%s'", msg, cr.Status.State))
	if err := r.Status().Update(context.Background(), cr); err != nil {
		log.Error(err, "unable to update clusterregistration status")
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *ClusterReconciler) ReconcileDelete(
	ctx context.Context,
	log logr.Logger,
	cr *arlonv1.Cluster,
	patchHelper *patch.Helper,
) (ctrl.Result, error) {
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Cluster{}).
		Complete(r)
}
