package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"go.chief.tools/serverchief-agent/optout"
)

func TestDisableCapabilityActionValidatesItsArgument(t *testing.T) {
	for _, args := range []map[string]string{
		nil,
		{"capability": "update_agent"},
		{"capability": "reboot", "also": "x"},
		{"name": "reboot"},
	} {
		if err := validateCapabilityArgs(args); err == nil {
			t.Errorf("%v: accepted", args)
		}
	}
	if err := validateCapabilityArgs(map[string]string{"capability": optout.UpgradePackages}); err != nil {
		t.Fatal(err)
	}
}

func TestDisableCapabilityActionAddsAndNeverRemoves(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disabled-capabilities.json")
	if err := optout.Save(path, []string{optout.UpgradePackages}); err != nil {
		t.Fatal(err)
	}
	action := disableCapabilityAction(path, -1)

	result := action.Run(context.Background(), map[string]string{"capability": optout.Reboot})
	var data disableCapabilityResult
	if result.Error != "" || json.Unmarshal(result.Data, &data) != nil || !data.Changed {
		t.Fatalf("result = %+v", result)
	}
	if state := optout.Load(path, -1); !slices.Equal(state.Disabled, []string{optout.Reboot, optout.UpgradePackages}) {
		t.Fatalf("disabled = %v", state.Disabled)
	}

	result = action.Run(context.Background(), map[string]string{"capability": optout.UpgradePackages})
	if result.Error != "" || json.Unmarshal(result.Data, &data) != nil || data.Changed {
		t.Fatalf("again = %+v", result)
	}
	if state := optout.Load(path, -1); !slices.Equal(state.Disabled, []string{optout.Reboot, optout.UpgradePackages}) {
		t.Fatalf("disabled after again = %v", state.Disabled)
	}
}
