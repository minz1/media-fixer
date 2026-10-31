package stuckread

import (
	"path"
	"strings"
)

func Title(p string) string {
	for _, root := range []struct {
		prefix string
		skip   int
	}{{"/mnt/decypharr/__all__/", 0}, {"/data/downloads/", 1}} {
		rest, ok := strings.CutPrefix(p, root.prefix)
		if !ok {
			continue
		}
		parts := strings.Split(rest, "/")
		if len(parts) >= root.skip+2 {
			return parts[root.skip]
		}
	}
	return path.Base(p)
}
