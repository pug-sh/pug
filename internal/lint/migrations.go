package lint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var migrationPrefix = regexp.MustCompile(`^(\d+)_`)

func checkMigrationNumbering(root string) ([]string, error) {
	var out []string
	for _, dir := range []string{"schema/postgres/migrations", "schema/clickhouse/migrations"} {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
			return nil, err
		}
		seen := map[int]string{}
		var nums []int
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
				continue
			}
			m := migrationPrefix.FindStringSubmatch(e.Name())
			if m == nil {
				out = append(out, fmt.Sprintf("%s/%s: migration name must start with a number", dir, e.Name()))
				continue
			}
			n, _ := strconv.Atoi(m[1])
			if prev, dup := seen[n]; dup {
				out = append(out, fmt.Sprintf("%s: migration %d is used twice (%s and %s)", dir, n, prev, e.Name()))
				continue
			}
			seen[n] = e.Name()
			nums = append(nums, n)
		}
		sort.Ints(nums)
		for i, n := range nums {
			if n != i+1 {
				out = append(out, fmt.Sprintf("%s: migration numbering has a gap at %d (%s)", dir, i+1, seen[n]))
				break
			}
		}
	}
	return out, nil
}
