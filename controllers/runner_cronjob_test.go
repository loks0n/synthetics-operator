package controllers

import (
	"maps"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

// Both CronJob-backed kinds must stamp the resolved selector onto the pod
// template — the merge itself is covered by TestRunnerNodeSelector.
func TestMutateCronJobStampsRunnerNodeSelector(t *testing.T) {
	defaults := map[string]string{"workload": "monitoring"}
	override := map[string]string{"disk": "ssd"}
	want := map[string]string{"workload": "monitoring", "disk": "ssd"}

	t.Run("PlaywrightTest", func(t *testing.T) {
		reconciler := &PlaywrightTestReconciler{RunnerNodeSelector: defaults}
		test := &syntheticsv1alpha1.PlaywrightTest{
			ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "default"},
			Spec: syntheticsv1alpha1.PlaywrightTestSpec{
				Runner: &syntheticsv1alpha1.RunnerSpec{NodeSelector: override},
			},
		}

		cronJob := &batchv1.CronJob{}
		reconciler.mutateCronJob(cronJob, test, "*/5 * * * *")

		if got := cronJob.Spec.JobTemplate.Spec.Template.Spec.NodeSelector; !maps.Equal(got, want) {
			t.Fatalf("nodeSelector = %v, want %v", got, want)
		}
	})

	t.Run("K6Test", func(t *testing.T) {
		reconciler := &K6TestReconciler{RunnerNodeSelector: defaults}
		test := &syntheticsv1alpha1.K6Test{
			ObjectMeta: metav1.ObjectMeta{Name: "load", Namespace: "default"},
			Spec: syntheticsv1alpha1.K6TestSpec{
				K6Version: "0.49.0",
				Runner:    &syntheticsv1alpha1.RunnerSpec{NodeSelector: override},
			},
		}

		cronJob := &batchv1.CronJob{}
		reconciler.mutateCronJob(cronJob, test, "*/5 * * * *")

		if got := cronJob.Spec.JobTemplate.Spec.Template.Spec.NodeSelector; !maps.Equal(got, want) {
			t.Fatalf("nodeSelector = %v, want %v", got, want)
		}
	})
}
