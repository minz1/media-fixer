package opexec

import (
	"github.com/minz1/mediafixer/internal/client"
	"github.com/minz1/mediafixer/internal/operation"
)

func ReplaceResultFrom(op operation.Op) client.ReplaceResult {
	var res client.ReplaceResult
	for i, st := range op.Steps {
		if i >= len(op.Results) || op.Results[i].State != operation.ResultDone {
			continue
		}
		switch st.Kind {
		case operation.StepBlocklist:
			res.BlocklistedGrabs = append(res.BlocklistedGrabs, st.HistoryID)
		case operation.StepDeleteFile:
			res.DeletedFiles = append(res.DeletedFiles, client.ReplaceFile{ID: st.FileID, Path: st.Path, Size: st.Size})
		case operation.StepSearch:
			res.SearchTriggered = st.Command
		case operation.StepQueueRemove:
		}
	}
	return res
}
