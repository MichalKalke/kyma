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

package tracepipeline

import (
	"context"
	"fmt"
	"github.com/kyma-project/kyma/components/telemetry-operator/internal/configchecksum"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	telemetryv1alpha1 "github.com/kyma-project/kyma/components/telemetry-operator/apis/telemetry/v1alpha1"
	"github.com/kyma-project/kyma/components/telemetry-operator/controller"
	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

type Config struct {
	CreateServiceMonitor bool
	CollectorNamespace   string
	ResourceName         string
	CollectorImage       string
}

type Reconciler struct {
	client.Client
	config Config
	Scheme *runtime.Scheme
}

func NewReconciler(client client.Client, config Config, scheme *runtime.Scheme) *Reconciler {
	var r Reconciler
	r.Client = client
	r.config = config
	r.Scheme = scheme
	return &r
}

func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	newReconciler := ctrl.NewControllerManagedBy(mgr).
		For(&telemetryv1alpha1.TracePipeline{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Secret{}).
		Owns(&corev1.Service{}).
		Watches(
			&source.Kind{Type: &corev1.Secret{}},
			handler.EnqueueRequestsFromMapFunc(r.enqueueRequests),
			builder.WithPredicates(predicate.Funcs{
				CreateFunc: func(createEvent event.CreateEvent) bool { return false },
				DeleteFunc: func(deleteEvent event.DeleteEvent) bool { return false },
				// only handle rotation of existing secrets
				UpdateFunc: func(updateEvent event.UpdateEvent) bool {
					return true
				},
				GenericFunc: func(genericEvent event.GenericEvent) bool { return false },
			}),
		)

	if r.config.CreateServiceMonitor {
		newReconciler.Owns(&monitoringv1.ServiceMonitor{})
	}

	return newReconciler.Complete(r)
}

func (r *Reconciler) enqueueRequests(object client.Object) []reconcile.Request {
	secret := object.(*corev1.Secret)
	var pipelines telemetryv1alpha1.TracePipelineList
	err := r.List(context.Background(), &pipelines)
	if err != nil {
		if errors.IsNotFound(err) {
			return []reconcile.Request{}
		}
		ctrl.Log.Error(err, "Secret UpdateEvent: fetching TracePipelineList failed!", err.Error())
		return []reconcile.Request{}
	}

	ctrl.Log.V(1).Info(fmt.Sprintf("Secret UpdateEvent: handling Secret: %s", secret.Name))
	var requests []reconcile.Request
	for i := range pipelines.Items {
		var p = pipelines.Items[i]
		if containsAnyRefToSecret(&p, secret) {
			request := reconcile.Request{NamespacedName: types.NamespacedName{Name: p.Name}}
			requests = append(requests, request)
			ctrl.Log.V(1).Info(fmt.Sprintf("Secret UpdateEvent: added reconcile request for pipeline: %s", p.Name))
		}
	}
	return requests
}

//+kubebuilder:rbac:groups=telemetry.kyma-project.io,resources=tracepipelines,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=telemetry.kyma-project.io,resources=tracepipelines/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=core,resources=configmaps,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;create;update;patch;delete

func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	logger.Info("Reconciliation triggered")

	var tracePipeline telemetryv1alpha1.TracePipeline
	if err := r.Get(ctx, req.NamespacedName, &tracePipeline); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	err := r.installOrUpgradeOtelCollector(ctx, &tracePipeline)
	return ctrl.Result{Requeue: controller.ShouldRetryOn(err)}, err
}

func (r *Reconciler) installOrUpgradeOtelCollector(ctx context.Context, tracing *telemetryv1alpha1.TracePipeline) error {
	var err error

	var secretData map[string][]byte
	if secretData, err = fetchSecretData(ctx, r, tracing.Spec.Output.Otlp); err != nil {
		return err
	}
	secret := makeSecret(r.config, secretData)
	if err = controllerutil.SetControllerReference(tracing, secret, r.Scheme); err != nil {
		return err
	}
	if err = createOrUpdateSecret(ctx, r.Client, secret); err != nil {
		return err
	}

	configMap := makeConfigMap(r.config, tracing.Spec.Output)
	if err = controllerutil.SetControllerReference(tracing, configMap, r.Scheme); err != nil {
		return err
	}
	if err = createOrUpdateConfigMap(ctx, r.Client, configMap); err != nil {
		return fmt.Errorf("failed to create otel collector configmap: %w", err)
	}

	configHash := configchecksum.Calculate([]corev1.ConfigMap{*configMap}, []corev1.Secret{*secret})
	deployment := makeDeployment(r.config, configHash)
	if err = controllerutil.SetControllerReference(tracing, deployment, r.Scheme); err != nil {
		return err
	}
	if err = createOrUpdateDeployment(ctx, r.Client, deployment); err != nil {
		return fmt.Errorf("failed to create otel collector deployment: %w", err)
	}

	service := makeCollectorService(r.config)
	if err = controllerutil.SetControllerReference(tracing, service, r.Scheme); err != nil {
		return err
	}
	if err = createOrUpdateService(ctx, r.Client, service); err != nil {
		return fmt.Errorf("failed to create otel collector service: %w", err)
	}

	if r.config.CreateServiceMonitor {
		serviceMonitor := makeServiceMonitor(r.config)
		if err = controllerutil.SetControllerReference(tracing, serviceMonitor, r.Scheme); err != nil {
			return err
		}

		if err = createOrUpdateServiceMonitor(ctx, r.Client, serviceMonitor); err != nil {
			return fmt.Errorf("failed to create otel collector prometheus service monitor: %w", err)
		}

		metricsService := makeMetricsService(r.config)
		if err = controllerutil.SetControllerReference(tracing, metricsService, r.Scheme); err != nil {
			return err
		}
		if err = createOrUpdateService(ctx, r.Client, metricsService); err != nil {
			return fmt.Errorf("failed to create otel collector metrics service: %w", err)
		}
	}

	return nil
}

func containsAnyRefToSecret(pipeline *telemetryv1alpha1.TracePipeline, secret *corev1.Secret) bool {
	secretName := types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}
	if pipeline.Spec.Output.Otlp.Endpoint.IsDefined() &&
		pipeline.Spec.Output.Otlp.Endpoint.ValueFrom != nil &&
		pipeline.Spec.Output.Otlp.Endpoint.ValueFrom.IsSecretKeyRef() &&
		pipeline.Spec.Output.Otlp.Endpoint.ValueFrom.SecretKeyRef.NamespacedName() == secretName {
		return true
	}

	if pipeline.Spec.Output.Otlp == nil ||
		pipeline.Spec.Output.Otlp.Authentication == nil ||
		pipeline.Spec.Output.Otlp.Authentication.Basic == nil ||
		!pipeline.Spec.Output.Otlp.Authentication.Basic.IsDefined() {
		return false
	}

	auth := pipeline.Spec.Output.Otlp.Authentication.Basic

	return (auth.User.ValueFrom.IsSecretKeyRef() && auth.User.ValueFrom.SecretKeyRef.NamespacedName() == secretName) ||
		(auth.Password.ValueFrom.IsSecretKeyRef() && auth.Password.ValueFrom.SecretKeyRef.NamespacedName() == secretName)
}
