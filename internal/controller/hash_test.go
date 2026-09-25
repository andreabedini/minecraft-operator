package controller

import (
	"crypto/sha1" //nolint:gosec // matching what Mojang publishes
	"crypto/sha512"
	"encoding/hex"
)

func sha1hex(s string) string {
	sum := sha1.Sum([]byte(s)) //nolint:gosec
	return hex.EncodeToString(sum[:])
}

func sha512hex(s string) string {
	sum := sha512.Sum512([]byte(s))
	return hex.EncodeToString(sum[:])
}
