package v1alpha1

import (
	"context"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func validHeartbeat(mutate func(*Heartbeat)) *Heartbeat {
	beat := &Heartbeat{
		ObjectMeta: metav1.ObjectMeta{Name: "db-backup", Namespace: "prod"},
		Spec: HeartbeatSpec{
			Period: metav1.Duration{Duration: time.Minute},
			Grace:  metav1.Duration{Duration: 3 * time.Minute},
		},
	}
	if mutate != nil {
		mutate(beat)
	}
	return beat
}

func TestHeartbeatValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Heartbeat)
		wantErr string
	}{
		{name: "valid"},
		{
			name:    "period is required",
			mutate:  func(h *Heartbeat) { h.Spec.Period = metav1.Duration{} },
			wantErr: "spec.period",
		},
		{
			name:    "period below the floor",
			mutate:  func(h *Heartbeat) { h.Spec.Period = metav1.Duration{Duration: time.Millisecond} },
			wantErr: "must be at least 1s",
		},
		{
			name:    "negative grace",
			mutate:  func(h *Heartbeat) { h.Spec.Grace = metav1.Duration{Duration: -time.Second} },
			wantErr: "spec.grace",
		},
		{
			name:   "zero grace is allowed",
			mutate: func(h *Heartbeat) { h.Spec.Grace = metav1.Duration{} },
		},
		{
			name:   "long period is allowed",
			mutate: func(h *Heartbeat) { h.Spec.Period = metav1.Duration{Duration: 7 * 24 * time.Hour} },
		},
		{
			name:    "tokenSecretRef without a name",
			mutate:  func(h *Heartbeat) { h.Spec.TokenSecretRef = &TokenSecretRef{Key: "token"} },
			wantErr: "spec.tokenSecretRef.name",
		},
		{
			name:    "tokenSecretRef with an invalid Secret name",
			mutate:  func(h *Heartbeat) { h.Spec.TokenSecretRef = &TokenSecretRef{Name: "Not Valid"} },
			wantErr: "must be a valid Secret name",
		},
		{
			name:    "tokenSecretRef with an invalid key",
			mutate:  func(h *Heartbeat) { h.Spec.TokenSecretRef = &TokenSecretRef{Name: "my-token", Key: "bad key"} },
			wantErr: "must be a valid Secret key",
		},
		{
			name:   "tokenSecretRef with defaults",
			mutate: func(h *Heartbeat) { h.Spec.TokenSecretRef = &TokenSecretRef{Name: "my-token"} },
		},
		{
			name:    "metricLabels colliding with a system label",
			mutate:  func(h *Heartbeat) { h.Spec.MetricLabels = map[string]string{"result": "nope"} },
			wantErr: "collides with a system label",
		},
		{
			name:   "metricLabels that are fine",
			mutate: func(h *Heartbeat) { h.Spec.MetricLabels = map[string]string{"team": "infra"} },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validHeartbeat(tc.mutate).validate(context.Background(), nil)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validate() = %v, want an error mentioning %q", err, tc.wantErr)
			}
		})
	}
}

// Grace defaults to Period rather than to zero: a zero window would report
// every heartbeat missed the instant its period elapsed.
func TestHeartbeatDefaultGraceMatchesPeriod(t *testing.T) {
	beat := validHeartbeat(func(h *Heartbeat) { h.Spec.Grace = metav1.Duration{} })
	if err := beat.Default(context.Background(), beat); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if beat.Spec.Grace.Duration != time.Minute {
		t.Fatalf("grace = %v, want it defaulted to the period (1m)", beat.Spec.Grace.Duration)
	}
}

func TestHeartbeatDefaultDoesNotOverrideAnExplicitGrace(t *testing.T) {
	beat := validHeartbeat(nil)
	if err := beat.Default(context.Background(), beat); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if beat.Spec.Grace.Duration != 3*time.Minute {
		t.Fatalf("grace = %v, want the explicit 3m", beat.Spec.Grace.Duration)
	}
}

func TestHeartbeatDefaultFillsTokenSecretKey(t *testing.T) {
	beat := validHeartbeat(func(h *Heartbeat) { h.Spec.TokenSecretRef = &TokenSecretRef{Name: "my-token"} })
	if err := beat.Default(context.Background(), beat); err != nil {
		t.Fatalf("Default: %v", err)
	}
	if beat.Spec.TokenSecretRef.Key != "token" {
		t.Fatalf("key = %q, want %q", beat.Spec.TokenSecretRef.Key, "token")
	}
}

// Changing the token source silently breaks every existing caller, and the
// symptom — a heartbeat going down — gives no hint why.
func TestHeartbeatUpdateWarnsOnTokenSourceChange(t *testing.T) {
	tests := []struct {
		name     string
		old, new *TokenSecretRef
		wantWarn bool
	}{
		{name: "unchanged generated"},
		{
			name:     "generated to supplied",
			new:      &TokenSecretRef{Name: "my-token", Key: "token"},
			wantWarn: true,
		},
		{
			name:     "supplied to generated",
			old:      &TokenSecretRef{Name: "my-token", Key: "token"},
			wantWarn: true,
		},
		{
			name:     "supplied to a different Secret",
			old:      &TokenSecretRef{Name: "my-token", Key: "token"},
			new:      &TokenSecretRef{Name: "other-token", Key: "token"},
			wantWarn: true,
		},
		{
			name: "unchanged supplied",
			old:  &TokenSecretRef{Name: "my-token", Key: "token"},
			new:  &TokenSecretRef{Name: "my-token", Key: "token"},
		},
	}

	validator := &HeartbeatValidator{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			old := validHeartbeat(func(h *Heartbeat) { h.Spec.TokenSecretRef = tc.old })
			updated := validHeartbeat(func(h *Heartbeat) { h.Spec.TokenSecretRef = tc.new })

			warnings, err := validator.ValidateUpdate(context.Background(), old, updated)
			if err != nil {
				t.Fatalf("ValidateUpdate: %v", err)
			}
			if got := len(warnings) > 0; got != tc.wantWarn {
				t.Fatalf("warnings = %v, wantWarn %v", warnings, tc.wantWarn)
			}
		})
	}
}

func TestHeartbeatDeadline(t *testing.T) {
	lastPing := metav1.NewTime(time.Unix(1700000000, 0).UTC())

	beat := validHeartbeat(nil)
	if got, want := beat.Deadline(lastPing).Time, lastPing.Add(4*time.Minute); !got.Equal(want) {
		t.Errorf("Deadline = %v, want %v", got, want)
	}

	// Zero grace must fall back to the period, matching the defaulter — the
	// two live in different places and drifting apart would be silent.
	noGrace := validHeartbeat(func(h *Heartbeat) { h.Spec.Grace = metav1.Duration{} })
	if got, want := noGrace.Deadline(lastPing).Time, lastPing.Add(2*time.Minute); !got.Equal(want) {
		t.Errorf("Deadline with zero grace = %v, want %v", got, want)
	}
}
