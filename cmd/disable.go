package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"go.chief.tools/serverchief-agent/agent"
	"go.chief.tools/serverchief-agent/helper"
	"go.chief.tools/serverchief-agent/optout"
)

// disableCapabilityResult is the data of the root helper's
// disable-capability action: whether it wrote the opt-out.
type disableCapabilityResult struct {
	Changed bool `json:"changed"`
}

// disableCapabilityAction is the root helper's disable-capability action:
// it adds one known capability to the opt-out at optOutPath, owned by
// owner (root), and never removes one.
func disableCapabilityAction(optOutPath string, owner int) helper.Handler {
	return helper.Handler{
		Timeout:  time.Minute,
		Validate: validateCapabilityArgs,
		Run: func(_ context.Context, args map[string]string) helper.Result {
			changed, err := optout.Disable(optOutPath, owner, args["capability"])
			if err != nil {
				return helper.Result{Error: err.Error()}
			}
			data, _ := json.Marshal(disableCapabilityResult{Changed: changed})
			return helper.Result{Data: data}
		},
	}
}

// validateCapabilityArgs accepts exactly {"capability": <known name>}.
func validateCapabilityArgs(args map[string]string) error {
	name, ok := args["capability"]
	if !ok || len(args) != 1 {
		return errors.New(`expected only "capability"`)
	}
	return optout.Validate([]string{name})
}

// newCapabilityDisabler turns a capability off through the root helper
// (or in-process when the daemon runs as root) and reports whether the
// opt-out changed.
func newCapabilityDisabler(privilege *agent.PrivilegeProbe) func(ctx context.Context, name string) (bool, error) {
	return func(ctx context.Context, name string) (bool, error) {
		result, err := privilege.Runner.Do(ctx, helper.ActionDisableCapability, map[string]string{"capability": name})
		if err != nil {
			return false, err
		}
		var decoded disableCapabilityResult
		if err := json.Unmarshal(result.Data, &decoded); err != nil {
			return false, errors.New("the root helper's disable-capability answer is unreadable")
		}
		return decoded.Changed, nil
	}
}
