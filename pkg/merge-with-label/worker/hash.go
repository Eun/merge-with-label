package worker

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/Eun/merge-with-label/pkg/merge-with-label/github"
)

func hashForKV(name string) string {
	h := sha512.Sum512([]byte(name))
	return hex.EncodeToString(h[:])
}

// prStateHash computes a deterministic SHA-256 fingerprint over all fields of
// PullRequestDetails that the worker uses to decide whether to merge or update
// a PR. Fields used only for logging, branch naming, or node IDs are excluded.
//
// Two calls with identical decision-relevant state return the same hash; any
// change to approvals, check results, labels, commit SHA, ahead-by count, or
// mergeability produces a different hash.
func prStateHash(details *github.PullRequestDetails) string {
	// Sort slices for determinism — GitHub may return items in arbitrary order.
	labels := make([]string, len(details.Labels))
	copy(labels, details.Labels)
	sort.Strings(labels)

	approvedBy := make([]string, len(details.ApprovedBy))
	copy(approvedBy, details.ApprovedBy)
	sort.Strings(approvedBy)

	// Flatten CheckStates map to a sorted "name=state" list.
	checkPairs := make([]string, 0, len(details.CheckStates))
	for name, state := range details.CheckStates {
		checkPairs = append(checkPairs, name+"="+state)
	}
	sort.Strings(checkPairs)

	input := strings.Join([]string{
		details.LastCommitSha,
		fmt.Sprintf("%d", details.AheadBy),
		strings.Join(labels, ","),
		strings.Join(approvedBy, ","),
		strings.Join(checkPairs, ","),
		fmt.Sprintf("%t", details.IsMergeable),
		fmt.Sprintf("%t", details.HasConflicts),
		details.MergeStateStatus,
	}, "|")

	h := sha256.Sum256([]byte(input))
	return hex.EncodeToString(h[:])
}
