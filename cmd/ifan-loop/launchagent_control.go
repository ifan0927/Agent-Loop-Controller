package main

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

const (
	defaultLaunchAgentControlTimeout = 15 * time.Second
	maxLaunchAgentControlTimeout     = 2 * time.Minute
	maxLaunchAgentPlistBytes         = 64 << 10
)

type launchAgentControlResult struct {
	Step           string `json:"step"`
	Label          string `json:"label"`
	ObservedState  string `json:"observed_state"`
	RunAtLoad      bool   `json:"run_at_load"`
	Outcome        string `json:"outcome"`
	NextSafeAction string `json:"next_safe_action"`
	Reason         string `json:"reason,omitempty"`
	TimedOut       bool   `json:"timed_out,omitempty"`
}

type launchAgentObservation struct {
	State string
}

type launchAgentCommandResult struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

type launchAgentControlError struct {
	Code string
}

func (e *launchAgentControlError) Error() string { return e.Code }

type launchAgentCommandRunner interface {
	Run(context.Context, []string) (launchAgentCommandResult, error)
}

type osLaunchAgentCommandRunner struct{}

func (osLaunchAgentCommandRunner) Run(ctx context.Context, args []string) (launchAgentCommandResult, error) {
	command := exec.CommandContext(ctx, "launchctl", args...)
	var stdout, stderr cappedLaunchAgentOutput
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	result := launchAgentCommandResult{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if command.ProcessState != nil {
		result.ExitCode = command.ProcessState.ExitCode()
	}
	if err != nil && ctx.Err() != nil {
		return result, ctx.Err()
	}
	if err != nil {
		if _, ok := err.(*exec.ExitError); ok {
			return result, nil
		}
		return result, err
	}
	return result, nil
}

type cappedLaunchAgentOutput struct {
	data      bytes.Buffer
	truncated bool
}

func (b *cappedLaunchAgentOutput) Write(value []byte) (int, error) {
	const limit = 64 << 10
	remaining := limit - b.data.Len()
	if remaining > 0 {
		if len(value) > remaining {
			_, _ = b.data.Write(value[:remaining])
			b.truncated = true
		} else {
			_, _ = b.data.Write(value)
		}
	} else {
		b.truncated = true
	}
	return len(value), nil
}

func (b *cappedLaunchAgentOutput) Bytes() []byte {
	return append([]byte(nil), b.data.Bytes()...)
}

type launchAgentControl interface {
	Status(context.Context, string) (launchAgentObservation, error)
	Bootstrap(context.Context, string, string) error
	Kickstart(context.Context, string) error
	Bootout(context.Context, string) error
}

type launchctlControl struct {
	runner  launchAgentCommandRunner
	timeout time.Duration
}

func newLaunchctlControl(timeout time.Duration) launchAgentControl {
	return launchctlControl{runner: osLaunchAgentCommandRunner{}, timeout: timeout}
}

var launchAgentControlFactory = newLaunchctlControl

func (c launchctlControl) run(ctx context.Context, args ...string) (launchAgentCommandResult, error) {
	if c.runner == nil || c.timeout <= 0 || c.timeout > maxLaunchAgentControlTimeout {
		return launchAgentCommandResult{}, &launchAgentControlError{Code: "control_invalid"}
	}
	stepCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	result, err := c.runner.Run(stepCtx, args)
	if stepCtx.Err() != nil {
		return result, &launchAgentControlError{Code: "control_timeout"}
	}
	if err != nil {
		return result, &launchAgentControlError{Code: "launchctl_unavailable"}
	}
	return result, nil
}

func (c launchctlControl) Status(ctx context.Context, target string) (launchAgentObservation, error) {
	result, err := c.run(ctx, "print", target)
	if err != nil {
		return launchAgentObservation{State: "unknown"}, err
	}
	if result.ExitCode != 0 {
		if launchctlReportsAbsent(result) {
			return launchAgentObservation{State: "absent"}, nil
		}
		return launchAgentObservation{State: "unknown"}, &launchAgentControlError{Code: "status_failed"}
	}
	return launchAgentObservation{State: normalizeLaunchAgentState(result.Stdout)}, nil
}

func (c launchctlControl) Bootstrap(ctx context.Context, domain, plist string) error {
	result, err := c.run(ctx, "bootstrap", domain, plist)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		if launchctlReportsAlreadyLoaded(result) {
			return nil
		}
		return &launchAgentControlError{Code: "bootstrap_failed"}
	}
	return nil
}

func (c launchctlControl) Kickstart(ctx context.Context, target string) error {
	result, err := c.run(ctx, "kickstart", target)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return &launchAgentControlError{Code: "kickstart_failed"}
	}
	return nil
}

func (c launchctlControl) Bootout(ctx context.Context, target string) error {
	result, err := c.run(ctx, "bootout", target)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 && !launchctlReportsAbsent(result) {
		return &launchAgentControlError{Code: "bootout_failed"}
	}
	return nil
}

func launchctlReportsAbsent(result launchAgentCommandResult) bool {
	if result.ExitCode == 113 {
		return true
	}
	output := strings.ToLower(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
	for _, marker := range []string{"could not find service", "service not found", "no such process", "unknown service"} {
		if strings.Contains(output, marker) {
			return true
		}
	}
	return false
}

func launchctlReportsAlreadyLoaded(result launchAgentCommandResult) bool {
	output := strings.ToLower(string(append(append([]byte(nil), result.Stdout...), result.Stderr...)))
	return strings.Contains(output, "already bootstrapped") || strings.Contains(output, "already loaded") || strings.Contains(output, "service already exists")
}

func normalizeLaunchAgentState(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		value, found := strings.CutPrefix(line, "state = ")
		if !found {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "running":
			return "running"
		case "waiting":
			return "waiting"
		case "spawn scheduled":
			return "scheduled"
		case "throttled":
			return "throttled"
		case "stopped", "not running", "exited":
			return "stopped"
		default:
			return "loaded"
		}
	}
	return "loaded"
}

type launchAgentPlistInspection struct {
	Label             string
	ProgramArguments  []string
	RunAtLoad         bool
	RunAtLoadObserved bool
}

func inspectLaunchAgentPlist(ctx context.Context, path string) (launchAgentPlistInspection, error) {
	data, err := readLaunchAgentFile(ctx, path)
	if err != nil {
		return launchAgentPlistInspection{}, err
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	token, err := nextPlistDocumentToken(decoder)
	if err != nil {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	root, ok := token.(xml.StartElement)
	if !ok || root.Name.Space != "" || root.Name.Local != "plist" || len(root.Attr) != 1 || root.Attr[0].Name.Space != "" || root.Attr[0].Name.Local != "version" || root.Attr[0].Value != "1.0" {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	token, err = nextPlistContentToken(decoder)
	if err != nil {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	dict, ok := token.(xml.StartElement)
	if !ok || !validPlistElement(dict, "dict") {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	inspection, err := decodeLaunchAgentPlistDict(decoder, dict, true)
	if err != nil {
		return launchAgentPlistInspection{}, err
	}
	token, err = nextPlistContentToken(decoder)
	if err != nil {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	end, ok := token.(xml.EndElement)
	if !ok || end.Name != root.Name {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	if _, err := nextPlistContentToken(decoder); !errors.Is(err, io.EOF) {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	return inspection, nil
}

func nextPlistDocumentToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		switch value := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(value)) == "" {
				continue
			}
		case xml.Comment, xml.Directive, xml.ProcInst:
			continue
		}
		return token, nil
	}
}

func nextPlistContentToken(decoder *xml.Decoder) (xml.Token, error) {
	for {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if value, ok := token.(xml.CharData); ok && strings.TrimSpace(string(value)) == "" {
			continue
		}
		if _, ok := token.(xml.Comment); ok {
			continue
		}
		return token, nil
	}
}

func validPlistElement(start xml.StartElement, name string) bool {
	if start.Name.Space != "" || start.Name.Local != name || len(start.Attr) != 0 {
		return false
	}
	return true
}

func decodeLaunchAgentPlistDict(decoder *xml.Decoder, start xml.StartElement, extract bool) (launchAgentPlistInspection, error) {
	if !validPlistElement(start, "dict") {
		return launchAgentPlistInspection{}, errors.New("plist_invalid")
	}
	seen := make(map[string]struct{})
	var inspection launchAgentPlistInspection
	for {
		token, err := nextPlistContentToken(decoder)
		if err != nil {
			return launchAgentPlistInspection{}, errors.New("plist_invalid")
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name != start.Name {
				return launchAgentPlistInspection{}, errors.New("plist_invalid")
			}
			return inspection, nil
		}
		keyStart, ok := token.(xml.StartElement)
		if !ok || !validPlistElement(keyStart, "key") {
			return launchAgentPlistInspection{}, errors.New("plist_invalid")
		}
		key, err := decodePlistTextElement(decoder, keyStart, "key")
		if err != nil {
			return launchAgentPlistInspection{}, err
		}
		if _, exists := seen[key]; exists {
			return launchAgentPlistInspection{}, errors.New("plist_invalid")
		}
		seen[key] = struct{}{}
		valueToken, err := nextPlistContentToken(decoder)
		if err != nil {
			return launchAgentPlistInspection{}, errors.New("plist_invalid")
		}
		valueStart, ok := valueToken.(xml.StartElement)
		if !ok {
			return launchAgentPlistInspection{}, errors.New("plist_invalid")
		}
		if !extract {
			if err := consumePlistValue(decoder, valueStart); err != nil {
				return launchAgentPlistInspection{}, err
			}
			continue
		}
		switch key {
		case "Label":
			if !validPlistElement(valueStart, "string") {
				return launchAgentPlistInspection{}, errors.New("plist_invalid")
			}
			inspection.Label, err = decodePlistTextElement(decoder, valueStart, "string")
		case "ProgramArguments":
			if !validPlistElement(valueStart, "array") {
				return launchAgentPlistInspection{}, errors.New("plist_invalid")
			}
			inspection.ProgramArguments, err = decodePlistStringArray(decoder, valueStart)
		case "RunAtLoad":
			if valueStart.Name.Space != "" || (valueStart.Name.Local != "true" && valueStart.Name.Local != "false") || len(valueStart.Attr) != 0 {
				return launchAgentPlistInspection{}, errors.New("plist_invalid")
			}
			err = consumePlistEmptyValue(decoder, valueStart)
			inspection.RunAtLoadObserved = true
			inspection.RunAtLoad = valueStart.Name.Local == "true"
		default:
			err = consumePlistValue(decoder, valueStart)
		}
		if err != nil {
			return launchAgentPlistInspection{}, err
		}
	}
}

func decodePlistStringArray(decoder *xml.Decoder, start xml.StartElement) ([]string, error) {
	var values []string
	for {
		token, err := nextPlistContentToken(decoder)
		if err != nil {
			return nil, errors.New("plist_invalid")
		}
		if end, ok := token.(xml.EndElement); ok {
			if end.Name != start.Name {
				return nil, errors.New("plist_invalid")
			}
			return values, nil
		}
		item, ok := token.(xml.StartElement)
		if !ok || !validPlistElement(item, "string") {
			return nil, errors.New("plist_invalid")
		}
		value, err := decodePlistTextElement(decoder, item, "string")
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
}

func consumePlistValue(decoder *xml.Decoder, start xml.StartElement) error {
	if start.Name.Space != "" || len(start.Attr) != 0 {
		return errors.New("plist_invalid")
	}
	switch start.Name.Local {
	case "dict":
		_, err := decodeLaunchAgentPlistDict(decoder, start, false)
		return err
	case "array":
		for {
			token, err := nextPlistContentToken(decoder)
			if err != nil {
				return errors.New("plist_invalid")
			}
			if end, ok := token.(xml.EndElement); ok {
				if end.Name != start.Name {
					return errors.New("plist_invalid")
				}
				return nil
			}
			item, ok := token.(xml.StartElement)
			if !ok {
				return errors.New("plist_invalid")
			}
			if err := consumePlistValue(decoder, item); err != nil {
				return err
			}
		}
	case "true", "false":
		return consumePlistEmptyValue(decoder, start)
	case "string", "integer", "real", "date", "data", "uid":
		_, err := decodePlistTextElement(decoder, start, start.Name.Local)
		return err
	default:
		return errors.New("plist_invalid")
	}
}

func consumePlistEmptyValue(decoder *xml.Decoder, start xml.StartElement) error {
	token, err := decoder.Token()
	if err != nil {
		return errors.New("plist_invalid")
	}
	end, ok := token.(xml.EndElement)
	if !ok || end.Name != start.Name {
		return errors.New("plist_invalid")
	}
	return nil
}

func decodePlistTextElement(decoder *xml.Decoder, start xml.StartElement, name string) (string, error) {
	if !validPlistElement(start, name) {
		return "", errors.New("plist_invalid")
	}
	var value strings.Builder
	for {
		token, err := decoder.Token()
		if err != nil {
			return "", errors.New("plist_invalid")
		}
		switch item := token.(type) {
		case xml.CharData:
			_, _ = value.Write(item)
		case xml.EndElement:
			if item.Name != start.Name {
				return "", errors.New("plist_invalid")
			}
			return value.String(), nil
		default:
			return "", errors.New("plist_invalid")
		}
	}
}

func readLaunchAgentFile(ctx context.Context, path string) ([]byte, error) {
	if !validLaunchAgentPath(path) {
		return nil, errors.New("plist_invalid")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("plist_unavailable")
	}
	return readLaunchAgentOpenedFile(ctx, file)
}

func readLaunchAgentOpenedFile(ctx context.Context, file *os.File) ([]byte, error) {
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maxLaunchAgentPlistBytes {
		return nil, errors.New("plist_invalid")
	}
	data := make([]byte, 0, info.Size())
	buffer := make([]byte, 16<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		read, readErr := file.Read(buffer)
		if read > 0 {
			data = append(data, buffer[:read]...)
			if len(data) > maxLaunchAgentPlistBytes {
				return nil, errors.New("plist_invalid")
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return nil, errors.New("plist_unavailable")
		}
	}
	return data, nil
}

func validateLaunchAgentPlist(ctx context.Context, options launchAgentOptions) (launchAgentPlistInspection, error) {
	if !launchAgentPathExists(options.plist) {
		return launchAgentPlistInspection{}, errors.New("plist_unavailable")
	}
	inspection, err := inspectLaunchAgentControlPlist(ctx, options)
	if err != nil {
		return launchAgentPlistInspection{}, err
	}
	if !inspection.RunAtLoad {
		return launchAgentPlistInspection{}, errors.New("plist_mismatch")
	}
	return inspection, nil
}

func inspectLaunchAgentControlPlist(ctx context.Context, options launchAgentOptions) (launchAgentPlistInspection, error) {
	if !safeLaunchAgentPlist(options.plist) {
		return launchAgentPlistInspection{}, errors.New("plist_unsafe")
	}
	inspection, err := inspectLaunchAgentPlist(ctx, options.plist)
	if err != nil {
		return launchAgentPlistInspection{}, err
	}
	wantArguments := []string{options.binary, "controller", "worker", "--config", options.config}
	if inspection.Label != launchAgentLabel || !inspection.RunAtLoadObserved || !sameStrings(inspection.ProgramArguments, wantArguments) {
		return launchAgentPlistInspection{}, errors.New("plist_mismatch")
	}
	return inspection, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func launchAgentTarget(options launchAgentOptions) string {
	return options.domain + "/" + launchAgentLabel
}

func launchAgentControlResultFor(options launchAgentOptions, step, state, outcome, next, reason string, runAtLoad bool, timedOut bool) launchAgentControlResult {
	return launchAgentControlResult{Step: step, Label: launchAgentLabel, ObservedState: state, RunAtLoad: runAtLoad, Outcome: outcome, NextSafeAction: next, Reason: reason, TimedOut: timedOut}
}

func launchAgentControlErrorCode(err error) (string, bool) {
	var controlErr *launchAgentControlError
	if errors.As(err, &controlErr) {
		return controlErr.Code, controlErr.Code == "control_timeout"
	}
	return "control_failed", false
}

func writeLaunchAgentControlResult(result launchAgentControlResult, err error) error {
	if printErr := printJSON(result); printErr != nil {
		return printErr
	}
	if err != nil {
		var controlErr *launchAgentControlError
		if errors.As(err, &controlErr) {
			return err
		}
		if result.Reason != "" {
			return &launchAgentControlError{Code: result.Reason}
		}
		return &launchAgentControlError{Code: "control_failed"}
	}
	return err
}
