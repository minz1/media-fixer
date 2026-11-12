package mediaagent

import "github.com/minz1/mediafixer/internal/mediaagentapi"

func NewProcOps(procRoot string, mounts []string) *RealOps {
	return &RealOps{mounts: mounts, procRoot: procRoot}
}

func ParseProbe(stdout, stderr []byte) (*mediaagentapi.ProbeResult, error) {
	return parseProbe(stdout, stderr)
}
