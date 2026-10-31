package babysit

type BundleInput = bundleInput

const SystemPrompt = systemPrompt

func BuildFacts(in BundleInput) string { return buildFacts(in) }

func Fingerprint(model, facts string) string { return fingerprint(model, facts) }
