package utils

import (
	crand "crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

func SplitNoEmpty(s, sep string) []string {
	if s == "" {
		return []string{}
	}

	res := strings.Split(s, sep)
	for i := 0; i < len(res); i++ {
		if res[i] == "" {
			res = append(res[:i], res[i+1:]...)
		}
	}

	return res
}

func JoinNoEmpty(s []string, sep string) string {
	if len(s) == 0 {
		return ""
	}

	return strings.Join(s, sep)
}

// RandomString returns a cryptographically-random string of length n over
// [a-z0-9]. It is used for security-relevant values such as the federation
// secret nonce, so it must not use a predictable (math/rand) source.
func RandomString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"

	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[RandomInt(0, len(letterBytes))]
	}
	return string(b)
}

// RandomInt returns a cryptographically-random int in [min, max) using unbiased
// rejection sampling via crypto/rand. It panics only if the system CSPRNG is
// unavailable, which is not a recoverable condition for a security primitive.
func RandomInt(min, max int) int {
	if max <= min {
		return min
	}
	r, err := crand.Int(crand.Reader, big.NewInt(int64(max-min)))
	if err != nil {
		panic(fmt.Sprintf("crypto/rand failure: %v", err))
	}
	return min + int(r.Int64())
}
