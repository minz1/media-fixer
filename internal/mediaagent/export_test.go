package mediaagent

func NewProcOps(procRoot string, mounts []string) *RealOps {
	return &RealOps{mounts: mounts, procRoot: procRoot}
}
