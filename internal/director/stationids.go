package director

import (
	"log/slog"
	"os"
	"strings"
	"sync"

	"github.com/the-algovn/radio-service/internal/brain"
)

// stationIDs rotates the committed station-ID scripts (persona/
// station-ids.txt, one variant per line; # comments and blanks ignored).
// Lines are validated at load with the on-air rules (digit-lint + rune cap);
// invalid lines are dropped with a log line. A missing or empty file makes
// station_id quietly ineligible — logged once, never a crash. The rotation
// index is in-memory and resets on restart (spec §2).
type stationIDs struct {
	mu    sync.Mutex
	lines []string
	idx   int
}

func loadStationIDs(path string, maxChars int, logger *slog.Logger) *stationIDs {
	if logger == nil {
		logger = slog.Default()
	}
	s := &stationIDs{}
	b, err := os.ReadFile(path)
	if err != nil {
		logger.Warn("station-ids unavailable; station_id segments disabled", "path", path, "err", err)
		return s
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if v := brain.Validate(line, maxChars); len(v) > 0 {
			logger.Warn("station-id line dropped", "line", line, "violations", v)
			continue
		}
		s.lines = append(s.lines, line)
	}
	if len(s.lines) == 0 {
		logger.Warn("station-ids file has no valid lines; station_id segments disabled", "path", path)
	}
	return s
}

func (s *stationIDs) available() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines) > 0
}

func (s *stationIDs) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lines) == 0 {
		return "", false
	}
	line := s.lines[s.idx%len(s.lines)]
	s.idx++
	return line, true
}
