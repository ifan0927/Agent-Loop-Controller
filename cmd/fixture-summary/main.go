package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ifan0927/Agent-Loop-Controller/internal/fixtureevidence"
)

type testEvent struct {
	Action string `json:"Action"`
	Test   string `json:"Test"`
	Output string `json:"Output"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	expectedPath := flag.String("expected", "", "compare generated evidence with this summary")
	flag.Parse()
	pending := map[string][]fixtureevidence.Evidence{}
	var passed []fixtureevidence.Evidence
	scanner := bufio.NewScanner(os.Stdin)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		var event testEvent
		if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Test == "" {
			continue
		}
		if index := strings.Index(event.Output, fixtureevidence.Marker); index >= 0 {
			var evidence fixtureevidence.Evidence
			decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(event.Output[index+len(fixtureevidence.Marker):])))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&evidence); err != nil || evidence.Validate() != nil {
				return errors.New("fixture emitted invalid evidence")
			}
			pending[event.Test] = append(pending[event.Test], evidence)
		}
		if event.Action == "pass" {
			passed = append(passed, pending[event.Test]...)
			delete(pending, event.Test)
		}
		if event.Action == "fail" {
			return fmt.Errorf("fixture test failed: %s", event.Test)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	summary, err := fixtureevidence.Aggregate(passed)
	if err != nil {
		return err
	}
	if *expectedPath != "" {
		raw, readErr := os.ReadFile(*expectedPath)
		if readErr != nil {
			return readErr
		}
		var expected fixtureevidence.Summary
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(&expected); decodeErr != nil {
			return decodeErr
		}
		expectedRaw, _ := json.Marshal(expected)
		actualRaw, _ := json.Marshal(summary)
		if !bytes.Equal(expectedRaw, actualRaw) {
			return errors.New("continuous supervisor fixture summary does not match passing execution evidence")
		}
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(summary)
}
