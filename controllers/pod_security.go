package controllers

import corev1 "k8s.io/api/core/v1"

// hardenPodSpec runs a test Job's pod under the RuntimeDefault seccomp profile,
// as a non-root user, with no privilege escalation and no capabilities in any
// container, so the Jobs pass Pod Security "restricted" admission policies.
// Every image a test Job runs sets a numeric non-root USER, which the kubelet
// checks against runAsNonRoot.
func hardenPodSpec(spec *corev1.PodSpec) {
	nonRoot := true
	spec.SecurityContext = &corev1.PodSecurityContext{
		RunAsNonRoot:   &nonRoot,
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
