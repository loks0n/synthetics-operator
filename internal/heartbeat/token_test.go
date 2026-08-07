package heartbeat

import (
	"strings"
	"testing"
)

func TestNewTokenShape(t *testing.T) {
	token, err := NewToken()
	if err != nil {
		t.Fatalf("NewToken: %v", err)
	}
	if !strings.HasPrefix(token, TokenPrefix) {
		t.Errorf("token %q lacks the %q prefix", token, TokenPrefix)
	}
	if !ValidToken(token) {
		t.Errorf("freshly generated token %q fails ValidToken", token)
	}
	if err := AcceptableToken(token); err != nil {
		t.Errorf("freshly generated token %q fails AcceptableToken: %v", token, err)
	}
}

func TestNewTokenIsUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		token, err := NewToken()
		if err != nil {
			t.Fatalf("NewToken: %v", err)
		}
		if seen[token] {
			t.Fatalf("NewToken returned a duplicate: %q", token)
		}
		seen[token] = true
	}
}

func TestValidToken(t *testing.T) {
	tests := []struct {
		name  string
		token string
		want  bool
	}{
		{name: "generated shape", token: "hb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: true},
		{name: "missing prefix", token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", want: false},
		{name: "too short", token: "hb_aaaa", want: false},
		{name: "uppercase", token: "hb_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", want: false},
		{name: "outside base32 alphabet", token: "hb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa1", want: false},
		{name: "empty", token: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidToken(tc.token); got != tc.want {
				t.Fatalf("ValidToken(%q) = %v, want %v", tc.token, got, tc.want)
			}
		})
	}
}

func TestAcceptableToken(t *testing.T) {
	tests := []struct {
		name    string
		token   string
		wantErr bool
	}{
		{name: "generated", token: "hb_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{name: "bring your own with unreserved characters", token: "my-own.token_value~1"},
		{name: "exactly at the floor", token: "0123456789abcdef"},
		{name: "one under the floor", token: "0123456789abcde", wantErr: true},
		{name: "contains a slash", token: "aaaaaaaa/bbbbbbbb", wantErr: true},
		{name: "contains a space", token: "aaaaaaaa bbbbbbbb", wantErr: true},
		{name: "percent encoded", token: "aaaaaaaa%2Fbbbbbbb", wantErr: true},
		{name: "empty", token: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := AcceptableToken(tc.token)
			if (err != nil) != tc.wantErr {
				t.Fatalf("AcceptableToken(%q) error = %v, wantErr %v", tc.token, err, tc.wantErr)
			}
		})
	}
}
