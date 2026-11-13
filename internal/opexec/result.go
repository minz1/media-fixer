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
		case operation.StepTorrentDelete, operation.StepTorrentAdd, operation.StepQueueRemove,
			operation.StepImport, operation.StepJellyfinNotify, operation.StepSubtitleSync:
		}
	}
	return res
}

func ReaddResultFrom(op operation.Op) client.ReaddResult {
	var res client.ReaddResult
	for i, st := range op.Steps {
		switch st.Kind {
		case operation.StepTorrentDelete:
			res.Name = st.Name
			res.Deleted = stepDone(op, i)
		case operation.StepTorrentAdd:
			res.Magnet = st.Magnet
			res.Readded = stepDone(op, i)
		case operation.StepBlocklist, operation.StepDeleteFile, operation.StepSearch, operation.StepQueueRemove,
			operation.StepImport, operation.StepJellyfinNotify, operation.StepSubtitleSync:
		}
	}
	return res
}

func stepDone(op operation.Op, i int) bool {
	return i < len(op.Results) && op.Results[i].State == operation.ResultDone
}

type SwapResult struct {
	Imported    bool `json:"imported"`
	Notified    bool `json:"notified"`
	Blocklisted bool `json:"blocklisted"`
}

func SwapResultFrom(op operation.Op) SwapResult {
	var res SwapResult
	for i, st := range op.Steps {
		switch st.Kind {
		case operation.StepImport:
			res.Imported = stepDone(op, i)
		case operation.StepJellyfinNotify:
			res.Notified = stepDone(op, i)
		case operation.StepBlocklist:
			res.Blocklisted = stepDone(op, i)
		case operation.StepDeleteFile, operation.StepSearch, operation.StepTorrentDelete, operation.StepTorrentAdd,
			operation.StepQueueRemove, operation.StepSubtitleSync:
		}
	}
	return res
}
