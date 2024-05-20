package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"time"

	trivy "github.com/aquasecurity/trivy-operator/pkg/apis/aquasecurity/v1alpha1"
	"github.com/go-logr/logr"
	"github.com/google/uuid"
	hosstedcomv1 "github.com/hossted/hossted-operator/api/v1"
	helm "github.com/hossted/hossted-operator/pkg/helm"
	internalHTTP "github.com/hossted/hossted-operator/pkg/http"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// HosstedProjectReconciler reconciles a HosstedProject object
type HosstedProjectReconciler struct {
	client.Client
	Scheme            *runtime.Scheme
	ReconcileDuration time.Duration
}

// +kubebuilder:rbac:groups=hossted.com,resources=hosstedprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=hossted.com,resources=hosstedprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=hossted.com,resources=hosstedprojects/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=list;get;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=daemonsets,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=list;get;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aquasecurity.github.io,resources=vulnerabilityreports,verbs=get;list;watch

// Reconcile reconciles the Hosstedproject custom resource.
func (r *HosstedProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrl.Log.WithName("controllers").WithName("hosstedproject").WithName(req.Namespace)

	logger.Info("Reconciling")

	// Get Hosstedproject custom resource
	instance := &hosstedcomv1.Hosstedproject{}

	err := r.Get(ctx, req.NamespacedName, instance)
	if err != nil {
		if errors.IsNotFound(err) {
			// instance not found check if req is of type VulnerabilityReport
			vr := &trivy.VulnerabilityReport{}
			err = r.Get(ctx, req.NamespacedName, vr)
			if err != nil {
				if errors.IsNotFound(err) {
					return ctrl.Result{}, nil
				}
				return ctrl.Result{}, err
			}
			// send vunerability report
			err = r.handleVulnReports(ctx, req.NamespacedName.Namespace, logger)
			if err != nil {
				return ctrl.Result{}, nil
			}
			return ctrl.Result{}, nil

		}
		return ctrl.Result{}, err
	}

	// Check if reconciliation should proceed
	if !instance.Spec.Stop {
		err = r.handleReconciliation(ctx, instance, logger)
		if err != nil {
			return ctrl.Result{}, err
		} else {
			return ctrl.Result{RequeueAfter: r.ReconcileDuration}, nil
		}
	}

	logger.Info("Reconciliation stopped", "name", req.Name)
	return ctrl.Result{RequeueAfter: r.ReconcileDuration}, nil
}

// handleReconciliation handles the reconciliation process.
func (r *HosstedProjectReconciler) handleReconciliation(ctx context.Context, instance *hosstedcomv1.Hosstedproject, logger logr.Logger) error {
	var err error
	var collector []*Collector
	var currentRevision []int
	var helmStatus []hosstedcomv1.HelmInfo

	if instance.Status.ClusterUUID == "" {
		clusterUUID := "K-" + uuid.NewString()
		instance.Status.ClusterUUID = clusterUUID
		// Update status
		if err := r.Status().Update(ctx, instance); err != nil {
			return err
		}
		collector, currentRevision, helmStatus, err = r.collector(ctx, instance)
		if err != nil {
			return err
		}
		err = r.handleNewCluster(ctx, instance, collector, currentRevision, helmStatus, logger)
		if err != nil {
			return err
		}

	}
	collector, currentRevision, helmStatus, err = r.collector(ctx, instance)
	if err != nil {
		return err
	}

	err = r.handleExistingCluster(ctx, instance, collector, currentRevision, helmStatus, logger)
	if err != nil {
		return err
	}

	return nil

}

// handleNewCluster handles reconciliation when a new cluster is created.
func (r *HosstedProjectReconciler) handleNewCluster(ctx context.Context, instance *hosstedcomv1.Hosstedproject, collector []*Collector, currentRevision []int, helmStatus []hosstedcomv1.HelmInfo, logger logr.Logger) error {

	sort.Ints(currentRevision)

	instance.Status.HelmStatus = helmStatus
	instance.Status.EmailID = os.Getenv("EMAIL_ID")
	instance.Status.LastReconciledTimestamp = time.Now().String()
	instance.Status.Revision = currentRevision

	// Update status
	if err := r.Status().Update(ctx, instance); err != nil {
		return err
	}

	if err := r.registerClusterUUID(instance, instance.Status.ClusterUUID, logger); err != nil {
		return err
	}

	if err := r.registerApps(instance, collector, logger); err != nil {
		return err
	}

	return nil
}

// handleExistingCluster handles reconciliation for an existing cluster.
func (r *HosstedProjectReconciler) handleExistingCluster(ctx context.Context, instance *hosstedcomv1.Hosstedproject, collector []*Collector, currentRevision []int, helmStatus []hosstedcomv1.HelmInfo, logger logr.Logger) error {
	if !compareSlices(instance.Status.Revision, currentRevision) {
		if err := r.registerApps(instance, collector, logger); err != nil {
			return err
		}

		// Update instance status
		instance.Status.HelmStatus = helmStatus
		instance.Status.Revision = currentRevision
		instance.Status.LastReconciledTimestamp = time.Now().String()

		// Update status
		if err := r.Status().Update(ctx, instance); err != nil {
			return err
		}

		return nil
	}

	err := r.handleMonitoring(ctx, instance)
	if err != nil {
		return err
	}

	logger.Info("No state change detected, requeueing")
	return nil
}

// registerApps registers applications with the Hossted API.
func (r *HosstedProjectReconciler) registerApps(instance *hosstedcomv1.Hosstedproject, collector []*Collector, logger logr.Logger) error {

	b, _ := json.Marshal(collector)
	fmt.Println(string(b))
	for i := range collector {
		collector[i].AppAPIInfo.ClusterUUID = instance.Status.ClusterUUID
		collectorJson, err := json.Marshal(collector[i])
		if err != nil {
			return err
		}

		appUUIDRegPath := os.Getenv("HOSSTED_API_URL") + "/apps"
		resp, err := internalHTTP.HttpRequest(collectorJson, appUUIDRegPath)
		if err != nil {
			return err
		}

		logger.Info(fmt.Sprintf("Sending req no [%d] to hossted API", i), "appUUID", collector[i].AppAPIInfo.AppUUID, "resp", resp.ResponseBody, "statuscode", resp.StatusCode)
	}

	return nil
}

// registerClusterUUID registers the cluster UUID with the Hossted API.
func (r *HosstedProjectReconciler) registerClusterUUID(instance *hosstedcomv1.Hosstedproject, clusterUUID string, logger logr.Logger) error {
	clusterUUIDRegPath := os.Getenv("HOSSTED_API_URL") + "/clusters/" + clusterUUID + "/register"

	type clusterUUIDBody struct {
		Email        string `json:"email"`
		ReqType      string `json:"type"`
		OrgID        string `json:"org_id"`
		ContextName  string `json:"context_name"`
		OptionsState struct {
			Monitoring bool `json:"monitoring"`
			Logging    bool `json:"logging"`
			CVE        bool `json:"cve_scan"`
		} `json:"options_state"`
	}

	clusterUUIDBodyReq := clusterUUIDBody{
		Email:       instance.Status.EmailID,
		ReqType:     "k8s",
		OrgID:       os.Getenv("HOSSTED_ORG_ID"),
		ContextName: os.Getenv("CONTEXT_NAME"),
		OptionsState: struct {
			Monitoring bool `json:"monitoring"`
			Logging    bool `json:"logging"`
			CVE        bool `json:"cve_scan"`
		}{
			Monitoring: instance.Spec.Monitoring.Enable,
			Logging:    instance.Spec.Logging.Enable,
			CVE:        instance.Spec.CVE.Enable,
		},
	}

	body, err := json.Marshal(clusterUUIDBodyReq)
	if err != nil {
		return err
	}

	resp, err := internalHTTP.HttpRequest(body, clusterUUIDRegPath)
	if err != nil {
		return err
	}

	logger.Info(fmt.Sprintf("Registering K8s with UUID no [%s] to hossted API", clusterUUID), "body", string(body), "resp", resp.ResponseBody, "statuscode", resp.StatusCode)

	return nil
}

// enable monitoring using grafana-agent.
func (r *HosstedProjectReconciler) handleMonitoring(ctx context.Context, instance *hosstedcomv1.Hosstedproject) error {
	// Helm configuration for Grafana Agent
	enableLog := "false"
	if instance.Spec.Logging.Enable {
		enableLog = "true"
	}
	h := helm.Helm{
		ChartName: "hossted-grafana-agent",
		RepoName:  "grafana",
		RepoUrl:   "https://charts.hossted.com",
		Namespace: "hossted-platform",
		Values: []string{
			"uuid=" + instance.Status.ClusterUUID,
			"logging.enabled=" + enableLog,
			"logging.url=" + os.Getenv("LOKI_URL"),
			"logging.username" + os.Getenv("LOKI_USERNAME"),
			"logging.password" + os.Getenv("LOKI_PASSWORD"),
			"metrics.url=" + os.Getenv("MIMIR_URL"),
			"metrics.username=" + os.Getenv("MIMIR_USERNAME"),
			"metrics.password=" + os.Getenv("MIMIR_PASSWORD"),
		},
	}

	ksm := helm.Helm{
		ChartName: "kube-state-metrics",
		RepoName:  "grafana",
		RepoUrl:   "https://prometheus-community.github.io/helm-charts",
		Namespace: "hossted-platform",
		Values: []string{
			"selfMonitor.enabled=true",
		},
	}

	// Check if monitoring is enabled
	if instance.Spec.Monitoring.Enable {
		// Check if Grafana Agent release already exists
		ok, err := helm.ListRelease(h.ChartName, h.Namespace)
		if err != nil {
			return err
		}

		if !ok {

			// install kubestate metrics
			// Install Grafana Agent
			err = helm.Apply(h)
			if err != nil {
				fmt.Println("grafana-agent for monitoring failed %w", err)
			}
			err = helm.Apply(ksm)
			if err != nil {
				fmt.Println("ksm-agent for monitoring failed %w", err)
			}
			return nil
		}

	} else {
		// If monitoring is not enabled, check if Grafana Agent release exists
		ok, err := helm.ListRelease(h.ChartName, h.Namespace)
		if err != nil {
			return err
		}
		if ok {
			err_del := helm.DeleteRelease(ksm.ChartName, ksm.Namespace)
			if err_del != nil {
				return fmt.Errorf("kube state metrics deletion failed %w", err_del)
			}

			// Delete Grafana Agent release if it exists
			err_del = helm.DeleteRelease(h.ChartName, h.Namespace)
			if err_del != nil {
				return fmt.Errorf("grafana Agent deletion failed %w", err_del)
			}

			err := r.Client.Delete(ctx, &appsv1.DaemonSet{
				ObjectMeta: v1.ObjectMeta{
					Name:      h.ChartName,
					Namespace: h.Namespace,
				},
			})
			if err != nil {
				return fmt.Errorf("failed to delete DaemonSet: %w", err)
			}
		}
	}
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *HosstedProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {

	return ctrl.NewControllerManagedBy(mgr).
		For(&hosstedcomv1.Hosstedproject{}).
		Watches(
			&trivy.VulnerabilityReport{},
			// handler.EnqueueRequestsFromMapFunc(r.findObjectsForVR),
			handler.Funcs{
				CreateFunc: func(ctx context.Context, e event.CreateEvent, q workqueue.RateLimitingInterface) {
					q.Add(reconcile.Request{NamespacedName: types.NamespacedName{
						Name:      e.Object.GetName(),
						Namespace: e.Object.GetNamespace(),
					}})
				},
			},
			builder.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
		).
		Complete(r)
}

// handleExistingCluster handles reconciliation for an existing cluster.
func (r *HosstedProjectReconciler) handleVulnReports(ctx context.Context, namespace string, logger logr.Logger) error {
	var err error
	var collector []*Collector

	instance := &hosstedcomv1.HosstedprojectList{}
	listOps := &client.ListOptions{}
	err = r.List(ctx, instance, listOps)
	if err != nil {
		return err
	}

	inst := &hosstedcomv1.Hosstedproject{}
	inst = &instance.Items[0]

	for _, ns := range inst.Spec.DenyNamespaces {
		if ns == namespace {
			return fmt.Errorf("Namespace in deny list for VR")
		}
	}

	collector, _, _, err = r.collector(ctx, inst)
	if err != nil {
		return err
	}

	if err := r.registerApps(inst, collector, logger); err != nil {
		return err
	}
	return nil
}
