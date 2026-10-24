package controllers

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	syntheticsv1alpha1 "github.com/loks0n/synthetics-operator/api/v1alpha1"
)

func TestTestCronJobsRunHardened(t *testing.T) {
	script := syntheticsv1alpha1.ScriptRef{ConfigMap: syntheticsv1alpha1.ConfigMapKeyRef{Name: "script", Key: "test.js"}}
	ttl := metav1.Duration{}

	playwright := &batchv1.CronJob{}
	(&PlaywrightTestReconciler{}).mutateCronJob(playwright, &syntheticsv1alpha1.PlaywrightTest{
		Spec: syntheticsv1alpha1.PlaywrightTestSpec{Script: script, TTLAfterFinished: ttl},
	}, "* * * * *")

	k6 := &batchv1.CronJob{}
	(&K6TestReconciler{}).mutateCronJob(k6, &syntheticsv1alpha1.K6Test{
		Spec: syntheticsv1alpha1.K6TestSpec{Script: script, TTLAfterFinished: ttl},
	}, "* * * * *")

	for name, cj := range map[string]*batchv1.CronJob{"playwright": playwright, "k6": k6} {
		spec := cj.Spec.JobTemplate.Spec.Template.Spec
		if spec.SecurityContext == nil || spec.SecurityContext.SeccompProfile == nil ||
			spec.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("%s: pod seccomp profile is not RuntimeDefault", name)
		}
		for _, c := range append(spec.InitContainers, spec.Containers...) {
			sc := c.SecurityContext
			if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
				t.Errorf("%s/%s: privilege escalation is allowed", name, c.Name)
			}
			if sc == nil || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
				t.Errorf("%s/%s: capabilities are not all dropped", name, c.Name)
			}
		}
	}
}
