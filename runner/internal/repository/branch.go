package repository

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

func BranchName(issueID, workflowID string) (string, error) {
	issue := branchComponent(issueID)
	if issue == "" || workflowID == "" {
		return "", errors.New("issue and workflow identifiers are required")
	}
	digest := sha256.Sum256([]byte(workflowID))
	branch := "agent/" + issue + "/" + hex.EncodeToString(digest[:])[:12]
	if !safeRef(branch) {
		return "", errors.New("generated branch name is invalid")
	}
	return branch, nil
}

func branchComponent(value string) string {
	var builder strings.Builder
	lastDash := false
	for _, runeValue := range strings.TrimSpace(value) {
		switch {
		case unicode.IsLetter(runeValue), unicode.IsDigit(runeValue):
			builder.WriteRune(unicode.ToLower(runeValue))
			lastDash = false
		case !lastDash:
			builder.WriteByte('-')
			lastDash = true
		}
		if builder.Len() >= 48 {
			break
		}
	}
	return strings.Trim(builder.String(), "-")
}
