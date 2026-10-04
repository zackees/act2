package runner

import "encoding/json"

type jobIdentityComponent struct {
	JobID  string          `json:"jobID"`
	Matrix json.RawMessage `json:"matrix"`
}

// jobPath identifies a job by the workflow job IDs of its callers followed by
// its own ID. Display names, matrix expansions, and workflow names do not enter
// this identity. A missing or invalid chain is omitted rather than guessed.
func (rc *RunContext) jobPath() []string {
	return identityPath(rc.jobIdentity())
}

func identityPath(identity []jobIdentityComponent) []string {
	if identity == nil {
		return nil
	}
	path := make([]string, len(identity))
	for i, component := range identity {
		path[i] = component.JobID
	}
	return path
}

// Capture each caller's matrix too: two caller legs can execute otherwise
// identical leaf jobs. RawMessage owns immutable JSON bytes, avoiding aliases
// to the runner's mutable expression environment.
func (rc *RunContext) jobIdentity() []jobIdentityComponent {
	const maxDepth = 32
	var reversed []jobIdentityComponent
	seen := make(map[*RunContext]struct{})
	for current := rc; current != nil; {
		if len(reversed) == maxDepth || current.Run == nil || current.Run.JobID == "" {
			return nil
		}
		if _, duplicate := seen[current]; duplicate {
			return nil
		}
		seen[current] = struct{}{}
		matrix, err := json.Marshal(current.Matrix)
		if err != nil {
			return nil
		}
		reversed = append(reversed, jobIdentityComponent{JobID: current.Run.JobID, Matrix: matrix})
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
