//go:build tools

package tools

import (
	_ "github.com/google/ko"
	_ "sigs.k8s.io/controller-runtime/tools/setup-envtest"
)
