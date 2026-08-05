package main

import (
	"maps"
	"testing"
)

func TestNodeSelectorFlagSet(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "single pair",
			args: []string{"workload=monitoring"},
			want: map[string]string{"workload": "monitoring"},
		},
		{
			name: "comma-separated pairs",
			args: []string{"workload=monitoring,disk=ssd"},
			want: map[string]string{"workload": "monitoring", "disk": "ssd"},
		},
		{
			name: "repeated flag accumulates",
			args: []string{"workload=monitoring", "disk=ssd"},
			want: map[string]string{"workload": "monitoring", "disk": "ssd"},
		},
		{
			name: "prefixed label key",
			args: []string{"node.kubernetes.io/instance-type=s-4vcpu-8gb"},
			want: map[string]string{"node.kubernetes.io/instance-type": "s-4vcpu-8gb"},
		},
		{
			name: "empty value selects nodes carrying the bare label",
			args: []string{"workload="},
			want: map[string]string{"workload": ""},
		},
		{
			name: "empty input is a no-op, so a chart can render the flag unconditionally",
			args: []string{""},
			want: map[string]string{},
		},
		{
			name:    "missing separator",
			args:    []string{"workload"},
			wantErr: true,
		},
		{
			name:    "invalid label key",
			args:    []string{"work load=monitoring"},
			wantErr: true,
		},
		{
			name:    "invalid label value",
			args:    []string{"workload=has spaces"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			selector := nodeSelectorFlag{}
			var err error
			for _, arg := range tc.args {
				if err = selector.Set(arg); err != nil {
					break
				}
			}

			if tc.wantErr {
				if err == nil {
					t.Fatalf("Set(%q) = nil error, want an error", tc.args)
				}
				return
			}
			if err != nil {
				t.Fatalf("Set(%q): %v", tc.args, err)
			}
			if !maps.Equal(selector, tc.want) {
				t.Fatalf("Set(%q) = %v, want %v", tc.args, selector, tc.want)
			}
		})
	}
}

// String is what `--help` prints and what flag defaults compare against, so it
// must round-trip into Set.
func TestNodeSelectorFlagStringIsSortedAndParsable(t *testing.T) {
	selector := nodeSelectorFlag{"workload": "monitoring", "disk": "ssd"}

	if got, want := selector.String(), "disk=ssd,workload=monitoring"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}

	reparsed := nodeSelectorFlag{}
	if err := reparsed.Set(selector.String()); err != nil {
		t.Fatalf("Set(String()): %v", err)
	}
	if !maps.Equal(reparsed, selector) {
		t.Fatalf("round-trip = %v, want %v", reparsed, selector)
	}
}
