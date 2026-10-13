package agent

import (
	"encoding/json"
	"fmt"
)

// RunnerPlacement stays in the intent payload so retries and queued work retain
// their requirements. Physical capacity is validated again by the controller.
type RunnerPlacement struct {
	NodeID      string            `json:"node_id,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Resources   struct {
		CPUMillis    int64  `json:"cpu_millis,omitempty"`
		MemoryMiB    int64  `json:"memory_mib,omitempty"`
		GPUCount     int    `json:"gpu_count,omitempty"`
		GPUModel     string `json:"gpu_model,omitempty"`
		GPUMemoryMiB int64  `json:"gpu_memory_mib,omitempty"`
	} `json:"resources,omitempty"`
}

func validateRunnerPlacement(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var p RunnerPlacement
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("runner requirements: %w", err)
	}
	if p.Resources.CPUMillis < 0 || p.Resources.MemoryMiB < 0 || p.Resources.GPUCount < 0 || p.Resources.GPUMemoryMiB < 0 {
		return fmt.Errorf("runner resource requirements must be nonnegative")
	}
	return nil
}
