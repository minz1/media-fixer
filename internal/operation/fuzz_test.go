package operation_test

import (
	"testing"

	"github.com/minz1/mediafixer/internal/machinetest"
)

func FuzzOperationMachine(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{0, 5, 8, 9, 12, 20, 21, 40, 41, 42, 50, 60})
	f.Add([]byte{1, 5, 14, 20, 40, 44, 5, 14, 20, 40, 45, 46, 47})
	f.Add([]byte{2, 255, 0, 3, 77, 130, 200, 31, 5, 6})
	m := opMachine()
	f.Fuzz(func(t *testing.T, data []byte) {
		machinetest.Sequence(t, m, data)
	})
}
