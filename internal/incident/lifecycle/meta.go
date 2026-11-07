package lifecycle

type Tone string

const (
	ToneYellow Tone = "yellow"
	ToneRed    Tone = "red"
	ToneBlue   Tone = "blue"
	TonePurple Tone = "purple"
	ToneOrange Tone = "orange"
	ToneGreen  Tone = "green"
	ToneGray   Tone = "gray"
)

type Driver string

const (
	DriverRun      Driver = "run"
	DriverVerify   Driver = "verify"
	DriverDownload Driver = "download"
	DriverPlan     Driver = "plan"
	DriverNone     Driver = "none"
)

type StateMeta struct {
	Label    string
	Line     string
	Tone     Tone
	Info     string
	Active   bool
	Live     bool
	Settled  bool
	Terminal bool
	Progress bool
	Driver   Driver
}

func Meta(s State) StateMeta {
	return metas()[s]
}

func StatesWhere(pred func(StateMeta) bool) []State {
	var out []State
	for _, s := range States() {
		if pred(Meta(s)) {
			out = append(out, s)
		}
	}
	return out
}

func metas() map[State]StateMeta {
	return map[State]StateMeta{
		Open: {
			Label: "open", Line: "🆕 Open", Tone: ToneYellow, Active: true, Live: true, Driver: DriverRun,
		},
		Reopened: {
			Label: "reopened", Line: "🔁 Re-opened", Tone: ToneRed, Active: true, Live: true, Driver: DriverRun,
		},
		Investigating: {
			Label: "investigating", Line: "🔁 Investigating…", Tone: ToneBlue,
			Active: true, Live: true, Progress: true, Driver: DriverRun,
		},
		Verifying: {
			Label: "verifying", Line: "⏳ Verifying the fix", Tone: TonePurple,
			Active: true, Progress: true, Driver: DriverVerify,
		},
		AwaitingDownload: {
			Label: "awaiting download", Line: "📥 Waiting for the download", Tone: TonePurple,
			Info:   "A search was sent; this checks the download until the file lands.",
			Active: true, Progress: true, Driver: DriverDownload,
		},
		ManualTestNeeded: {
			Label: "manual test needed", Line: "🧑‍🔧 Needs you", Tone: ToneOrange, Settled: true, Driver: DriverNone,
		},
		ExecutingPlan: {
			Label: "running plan", Line: "🚀 Running the approved plan", Tone: ToneBlue,
			Info:   "Your approved plan is running; Rerun is available when it finishes.",
			Active: true, Driver: DriverPlan,
		},
		AgentFixed: {
			Label: "agent fixed", Line: "✅ Fixed by the agent", Tone: ToneGreen, Settled: true, Driver: DriverNone,
		},
		Blocked: {
			Label: "blocked", Line: "⛔ Blocked", Tone: ToneRed, Settled: true, Driver: DriverNone,
		},
		None: {Driver: DriverNone},
		Resolved: {
			Label: "resolved", Line: "✅ Resolved", Tone: ToneGray, Settled: true, Terminal: true, Driver: DriverNone,
		},
	}
}
