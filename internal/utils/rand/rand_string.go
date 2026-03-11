package rand

import (
	"math/rand"
)

const letterBytes = "abcdefghijklmnopqrstuvwxyz0123456789"

// GenerateRandomString generates a random string of length n using lowercase letters and digits.
func GenerateRandomString(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[rand.Intn(len(letterBytes))]
	}
	return string(b)
}
