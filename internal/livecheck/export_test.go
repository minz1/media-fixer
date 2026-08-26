package livecheck

// CheckRegistryForTest exposes checkRegistry for internal/livecheck/coverage_test.go
// (package livecheck_test).
func CheckRegistryForTest() []checkSpec { return checkRegistry() }
