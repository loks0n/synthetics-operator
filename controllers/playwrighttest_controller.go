package controllers

import (
	"context"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
	internalmetrics "github.com/loks0n/synthetics-operator/internal/metrics"
)

type PlaywrightTestReconciler struct {
	client.Client
	Scheme                *runtime.Scheme
	Metrics               *internalmetrics.Store
	Clock                 func() time.Time
	NATSUrl               string
	TestSidecarImage      string
	PlaywrightRunnerImage string
}

func (r *PlaywrightTestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var test syntheticsv1alpha1.PlaywrightTest
	kind := string(syntheticsv1alpha1.DependencyKindPlaywrightTest)
	if err := r.Get(ctx, req.NamespacedName, &test); err != nil {
		if apierrors.IsNotFound(err) {
			r.Metrics.Delete(req.NamespacedName)
			r.Metrics.ClearDepends(kind, req.NamespacedName)
			r.Metrics.ClearMetricLabels(kind, req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !test.DeletionTimestamp.IsZero() {
		r.Metrics.Delete(req.NamespacedName)
		r.Metrics.ClearDepends(kind, req.NamespacedName)
		r.Metrics.ClearMetricLabels(kind, req.NamespacedName)
		return ctrl.Result{}, nil
	}

	r.Metrics.SetDepends(kind, req.NamespacedName, test.Spec.Depends)
	r.Metrics.SetMetricLabels(kind, req.NamespacedName, test.Spec.MetricLabels)

	original := test.DeepCopy()
	now := metav1.NewTime(r.Clock())

	test.Status.ObservedGeneration = test.Generation
	setSuspendedCondition(&test.Status.Conditions, test.Generation, test.Spec.Suspend, now)

	if err := r.reconcileCronJob(ctx, &test); err != nil {
		return ctrl.Result{}, err
	}

	if probeStatusChanged(original.Status.ObservedGeneration, test.Status.ObservedGeneration, original.Status.Conditions, test.Status.Conditions) {
		if err := r.Status().Patch(ctx, &test, client.MergeFrom(original)); err != nil {
			return ctrl.Result{}, err
		}
	}

	return ctrl.Result{}, nil
}

func (r *PlaywrightTestReconciler) reconcileCronJob(ctx context.Context, test *syntheticsv1alpha1.PlaywrightTest) error {
	schedule, ok := validSchedule(test.Namespace, test.Name, test.Spec.Interval.Duration)
	if !ok {
		return nil
	}

	cj := &batchv1.CronJob{ObjectMeta: metav1.ObjectMeta{Name: test.Name, Namespace: test.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cj, func() error {
		if err := controllerutil.SetControllerReference(test, cj, r.Scheme); err != nil {
			return err
		}
		r.mutateCronJob(cj, test, schedule)
		return nil
	})
	return err
}

func (r *PlaywrightTestReconciler) mutateCronJob(cj *batchv1.CronJob, test *syntheticsv1alpha1.PlaywrightTest, schedule string) {
	suspend := test.Spec.Suspend
	ttl := int32(test.Spec.TTLAfterFinished.Seconds())
	// A failed run is signal — don't retry. Retries double-publish to NATS
	// and duplicate metrics for the same logical failure.
	var backoffLimit int32

	cj.Spec.Schedule = schedule
	cj.Spec.Suspend = &suspend
	cj.Spec.ConcurrencyPolicy = batchv1.ForbidConcurrent
	cj.Spec.JobTemplate.Spec.TTLSecondsAfterFinished = &ttl
	cj.Spec.JobTemplate.Spec.BackoffLimit = &backoffLimit
	cj.Spec.JobTemplate.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	cj.Spec.JobTemplate.Spec.Template.Spec.Volumes = r.buildVolumes(test)
	cj.Spec.JobTemplate.Spec.Template.Spec.InitContainers = r.buildInitContainers()
	cj.Spec.JobTemplate.Spec.Template.Spec.Containers = r.buildRunnerContainers(test)

	if test.Spec.Runner != nil {
		cj.Spec.JobTemplate.Spec.Template.Spec.Affinity = test.Spec.Runner.Affinity
	}
}

func (r *PlaywrightTestReconciler) buildVolumes(test *syntheticsv1alpha1.PlaywrightTest) []corev1.Volume {
	return []corev1.Volume{
		{Name: "results", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{
			Name: "script",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: test.Spec.Script.ConfigMap.Name},
					Items:                []corev1.KeyToPath{{Key: test.Spec.Script.ConfigMap.Key, Path: "test.spec.js"}},
				},
			},
		},
	}
}

func (r *PlaywrightTestReconciler) buildInitContainers() []corev1.Container {
	sidecarRestartPolicy := corev1.ContainerRestartPolicyAlways
	sidecarArgs := []string{}
	if r.NATSUrl != "" {
		sidecarArgs = []string{"--nats-url=" + r.NATSUrl}
	}
	return []corev1.Container{
		{
			Name:          "test-sidecar",
			Image:         r.TestSidecarImage,
			Args:          sidecarArgs,
			RestartPolicy: &sidecarRestartPolicy,
			VolumeMounts: []corev1.VolumeMount{
				{Name: "results", MountPath: "/results"},
			},
		},
	}
}

func (r *PlaywrightTestReconciler) buildRunnerContainers(test *syntheticsv1alpha1.PlaywrightTest) []corev1.Container {
	env := []corev1.EnvVar{
		{Name: "PLAYWRIGHT_TEST_NAME", Value: test.Name},
		{Name: "PLAYWRIGHT_TEST_NAMESPACE", Value: test.Namespace},
	}
	var envFrom []corev1.EnvFromSource
	var resources corev1.ResourceRequirements
	if test.Spec.Runner != nil {
		env = append(test.Spec.Runner.Env, env...)
		envFrom = test.Spec.Runner.EnvFrom
		resources = test.Spec.Runner.Resources
	}
	return []corev1.Container{
		{
			Name:      "runner",
			Image:     r.PlaywrightRunnerImage,
			Env:       env,
			EnvFrom:   envFrom,
			Resources: resources,
			VolumeMounts: []corev1.VolumeMount{
				{Name: "results", MountPath: "/results"},
				{Name: "script", MountPath: "/scripts"},
			},
		},
	}
}

func (r *PlaywrightTestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&syntheticsv1alpha1.PlaywrightTest{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Owns(&batchv1.CronJob{}).
		Complete(r)
}
