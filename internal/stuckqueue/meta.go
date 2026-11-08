package stuckqueue

func Meta(s Snapshot) StateMeta {
	switch s.State {
	case StateUntracked:
		return StateMeta{Terminal: true, Tone: ToneNeutral, Drivers: []Driver{DriverRun}}
	case StateWaiting:
		return stored("Waiting", ToneNeutral, DriverRun)
	case StateDue:
		return stored("New", ToneNeutral, DriverRun)
	case StateHeld:
		return stored("Held", ToneNeutral, DriverRun)
	case StateNotified:
		return stored("Stuck", ToneWarn, DriverRun)
	case StateImportInflight:
		return stored("Importing", ToneNeutral, DriverHousekeeping)
	case StateActed:
		return actedMeta(s)
	case StateCleared:
		return stored("Cleared", ToneNeutral, DriverRun)
	}
	return StateMeta{}
}

func stored(label string, tone Tone, drivers ...Driver) StateMeta {
	return StateMeta{Stored: true, Label: label, Tone: tone, Drivers: drivers}
}

func actedMeta(s Snapshot) StateMeta {
	if unfinished(s, Event{}) {
		return stored("Unfinished", ToneWarn, DriverRun, DriverHousekeeping, DriverOwner)
	}
	label := "Acted"
	if s.Acted != nil {
		label = actionDone(s.Acted.Action)
	}
	return stored(label, ToneOK, DriverRun, DriverHousekeeping)
}
