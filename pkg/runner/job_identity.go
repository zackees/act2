package runner

// jobPath identifies a job by the workflow job IDs of its callers followed by
// its own ID. Display names, matrix expansions, and workflow names do not enter
// this identity. A missing or invalid chain is omitted rather than guessed.
func (rc *RunContext) jobPath() []string {
	const maxDepth = 32
	var reversed []string
	seen := make(map[*RunContext]struct{})
	for current := rc; current != nil; {
		if len(reversed) == maxDepth || current.Run == nil || current.Run.JobID == "" {
			return nil
		}
		if _, duplicate := seen[current]; duplicate {
			return nil
		}
		seen[current] = struct{}{}
		reversed = append(reversed, current.Run.JobID)
		if current.caller == nil {
			break
		}
		current = current.caller.runContext
		if current == nil {
			return nil
		}
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}
