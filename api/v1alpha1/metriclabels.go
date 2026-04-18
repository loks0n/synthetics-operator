package v1alpha1

import (
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
)

// promLabelNameRe matches Prometheus's permitted label name syntax. Keys that
// don't match this are rejected by Prometheus at scrape time, so admission-
// reject is kinder than producing a broken scrape.
var promLabelNameRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// isSystemLabel reports whether a label key is emitted by the operator
// itself. A user-supplied metricLabels key that collides with one of these
// would silently override operator-maintained state (like a probe's result
// or name), producing confusing observability — admission rejects the
// collision.
//
// Keeping the set as a switch means adding a label in store.go requires
// updating this function, and the reminder is deliberate.
func isSystemLabel(key string) bool {
	switch key {
	case "name", "namespace", "kind",
		"result", "failed_assertion",
		"url", "method", "phase",
		"assertion", "expr",
		"value", "type",
		"suite", "test",
		"unhealthy_dependency", "unhealthy_dependency_kind",
		// Prometheus / OTel well-known.
		"__name__", "le", "quantile", "job", "instance":
		return true
	}
	return false
}

// ValidateMetricLabels checks that each user-supplied label key is a valid
// Prometheus name and doesn't collide with an operator-emitted system label.
// Values are not validated — users are trusted to manage their own
// cardinality (see the docs for guidance).
func ValidateMetricLabels(labels map[string]string, path *field.Path) field.ErrorList {
	var allErrs field.ErrorList
	for key := range labels {
		fp := path.Key(key)
		if !promLabelNameRe.MatchString(key) {
			allErrs = append(allErrs, field.Invalid(fp, key, "must match Prometheus label name format [a-zA-Z_][a-zA-Z0-9_]*"))
			continue
		}
		if strings.HasPrefix(key, "__") {
			allErrs = append(allErrs, field.Invalid(fp, key, "labels starting with __ are reserved by Prometheus"))
			continue
		}
		if isSystemLabel(key) {
			allErrs = append(allErrs, field.Invalid(fp, key, "collides with a system label emitted by the operator"))
		}
	}
	return allErrs
}
