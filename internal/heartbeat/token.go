package heartbeat

import (
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"regexp"
	"strings"
)

// TokenPrefix marks a string as a heartbeat token. Purely cosmetic — it makes
// a leaked token recognisable in a log or a shell history, which is what you
// want when deciding whether to rotate one.
const TokenPrefix = "hb_"

// tokenEntropyBytes is the size of the random component. 20 bytes is 160
// bits, which encodes to 32 base32 characters and is far past brute-forcing
// over the public internet: an attacker who could try a million URLs a second
// would still need longer than the age of the universe.
const tokenEntropyBytes = 20

// tokenRe matches a well-formed token. The receiver checks this before doing
// a map lookup so that garbage paths are rejected on shape alone.
var tokenRe = regexp.MustCompile(`^hb_[a-z2-7]{32}$`)

// NewToken mints a token. Returns an error rather than panicking on a failed
// read from the system CSPRNG: the caller is a reconciler that should requeue,
// not die.
func NewToken() (string, error) {
	buf := make([]byte, tokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating heartbeat token: %w", err)
	}
	// Unpadded base32 over base64: the alphabet survives case-insensitive
	// handling — log pipelines that downcase, humans retyping from a terminal
	// — without changing the token.
	encoding := base32.StdEncoding.WithPadding(base32.NoPadding)
	return TokenPrefix + strings.ToLower(encoding.EncodeToString(buf)), nil
}

// ValidToken reports whether s has the shape of a generated token. A
// user-supplied token via spec.tokenSecretRef is not required to match — see
// AcceptableToken.
func ValidToken(s string) bool {
	return tokenRe.MatchString(s)
}

// minAcceptableTokenLength floors bring-your-own tokens. Short tokens are
// guessable, and a heartbeat that can be forged is worse than no heartbeat:
// it reports healthy while the job it watches is dead.
const minAcceptableTokenLength = 16

// AcceptableToken validates a token from any source, including one the user
// supplied through spec.tokenSecretRef. Looser than ValidToken — arbitrary
// URL-safe characters are allowed — but still enforces a length floor and
// rejects anything that would need escaping in a URL path.
func AcceptableToken(s string) error {
	if len(s) < minAcceptableTokenLength {
		return fmt.Errorf("token must be at least %d characters", minAcceptableTokenLength)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return fmt.Errorf("token contains %q; only unreserved URL characters (A-Z a-z 0-9 - _ . ~) are allowed", r)
		}
	}
	return nil
}
