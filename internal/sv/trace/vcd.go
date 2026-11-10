package trace

import (
	"bufio"
	"os"
	"sort"
	"strconv"
	"strings"
)

type timeValue struct {
	Time  int64
	Value string
	// Radix records how Value was dumped: 'b' binary, 's' signed binary,
	// 'r' real, or 0 for a single-bit scalar.
	Radix byte
}

type vcdData struct {
	Signals []string
	series  map[string][]timeValue
}

// parseVCD reads the signal definitions and value changes we need for the trace
// report. It intentionally ignores timescale, real values and dump commands.
func parseVCD(path string) (*vcdData, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	scopes := []string{}
	idNames := map[string][]string{}
	seriesByID := map[string][]timeValue{}
	currentTime := int64(0)
	inDefinitions := true

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "$scope"):
			parts := strings.Fields(line)
			if len(parts) >= 3 {
				scopes = append(scopes, parts[2])
			}
			continue
		case strings.HasPrefix(line, "$upscope"):
			if len(scopes) > 0 {
				scopes = scopes[:len(scopes)-1]
			}
			continue
		case strings.HasPrefix(line, "$var"):
			parts := strings.Fields(line)
			if len(parts) >= 5 {
				id := parts[3]
				name := parts[4]
				full := name
				if len(scopes) > 0 {
					full = strings.Join(append(append([]string{}, scopes...), name), ".")
				}
				idNames[id] = append(idNames[id], full)
			}
			continue
		case strings.HasPrefix(line, "$enddefinitions"):
			inDefinitions = false
			continue
		case strings.HasPrefix(line, "$"):
			continue
		case strings.HasPrefix(line, "#"):
			if value, err := strconv.ParseInt(line[1:], 10, 64); err == nil {
				currentTime = value
			}
			continue
		}

		if inDefinitions {
			continue
		}

		if strings.HasPrefix(line, "b") || strings.HasPrefix(line, "B") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				seriesByID[parts[1]] = append(seriesByID[parts[1]], timeValue{Time: currentTime, Value: parts[0][1:], Radix: 'b'})
			}
			continue
		}
		if strings.HasPrefix(line, "s") || strings.HasPrefix(line, "S") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				seriesByID[parts[1]] = append(seriesByID[parts[1]], timeValue{Time: currentTime, Value: parts[0][1:], Radix: 's'})
			}
			continue
		}
		if strings.HasPrefix(line, "r") || strings.HasPrefix(line, "R") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				seriesByID[parts[1]] = append(seriesByID[parts[1]], timeValue{Time: currentTime, Value: parts[0][1:], Radix: 'r'})
			}
			continue
		}
		if len(line) >= 2 {
			seriesByID[line[1:]] = append(seriesByID[line[1:]], timeValue{Time: currentTime, Value: line[:1], Radix: 0})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	series := map[string][]timeValue{}
	signals := []string{}
	for id, names := range idNames {
		for _, name := range names {
			series[name] = seriesByID[id]
			signals = append(signals, name)
		}
	}
	sort.Strings(signals)
	return &vcdData{Signals: signals, series: series}, nil
}
