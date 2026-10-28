// Package auth checks bearer tokens held in files, so a rotated Secret takes
// effect without a restart.
package auth

import (
	"crypto/subtle"
	"errors"
	"os"
	"strings"
)

// ReadToken reads a token file. An empty token is an error: it would let an
// empty bearer token in.
func ReadToken(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is chosen by the operator
	if err != nil {
		return nil, err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return nil, errors.New("token file is empty")
	}
	return []byte(token), nil
}

// BearerMatches reports, in constant time, whether an Authorization header
// carries want.
func BearerMatches(header string, want []byte) bool {
	scheme, got, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), want) == 1
}
