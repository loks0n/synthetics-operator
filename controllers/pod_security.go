package controllers

import corev1 "k8s.io/api/core/v1"

// hardenPodSpec runs a test Job's pod under the RuntimeDefault seccomp profile,
// with no privilege escalation and no capabilities in any container, so the
// Jobs pass Pod Security "restricted"-style admission policies.
func hardenPodSpec(spec *corev1.PodSpec) {
	spec.SecurityContext = &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
	for _, containers := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range containers {
			escalation := false
			containers[i].SecurityContext = &corev1.SecurityContext{
				AllowPrivilegeEscalation: &escalation,
				Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
			}
		}
	}
}
