package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

func FailureFingerprint(component string, err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"token", "secret", "credential", "password", "authorization"} {
		if index := strings.Index(message, marker); index >= 0 {
			message = message[:index] + marker
		}
	}
	for _, category := range []string{"workspace", "repository", "git", "pipeline", "agent", "executor", "disk", "environment"} {
		if strings.Contains(message, category) {
			component = category
			break
		}
	}
	digest := sha256.Sum256([]byte(component + "\n" + message))
	return component + ":" + hex.EncodeToString(digest[:8])
}
